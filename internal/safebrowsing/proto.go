package safebrowsing

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
)

// Wire format of google.security.safebrowsing.v5:
//
//	message SearchUrlsResponse { repeated ThreatUrl threats = 1; google.protobuf.Duration cache_duration = 2; }
//	message ThreatUrl          { string url = 1; repeated ThreatType threat_types = 2; }
//	enum ThreatType { THREAT_TYPE_UNSPECIFIED = 0; MALWARE = 1; SOCIAL_ENGINEERING = 2;
//	                  UNWANTED_SOFTWARE = 3; POTENTIALLY_HARMFUL_APPLICATION = 4; }
var threatTypeNames = map[uint64]string{
	0: "THREAT_TYPE_UNSPECIFIED",
	1: "MALWARE",
	2: "SOCIAL_ENGINEERING",
	3: "UNWANTED_SOFTWARE",
	4: "POTENTIALLY_HARMFUL_APPLICATION",
}

func threatTypeName(v uint64) string {
	if n, ok := threatTypeNames[v]; ok {
		return n
	}
	return fmt.Sprintf("THREAT_TYPE_%d", v)
}

// decodeSearchURLsResponse returns the de-duplicated threat types of a
// SearchUrlsResponse. Any ThreatUrl entry counts as a threat, even one
// without (or with unknown) threat types, so the result can only err on the
// unsafe side.
func decodeSearchURLsResponse(b []byte) ([]string, error) {
	var threats []string
	seen := map[string]bool{}
	add := func(t string) {
		if !seen[t] {
			seen[t] = true
			threats = append(threats, t)
		}
	}
	err := forEachField(b, func(num protowire.Number, typ protowire.Type, v []byte, n uint64) error {
		if num != 1 {
			return nil // cache_duration and unknown fields
		}
		if typ != protowire.BytesType {
			return errors.New("threats: unexpected wire type")
		}
		types, err := decodeThreatURL(v)
		if err != nil {
			return err
		}
		if len(types) == 0 {
			add(threatTypeName(0))
		}
		for _, t := range types {
			add(threatTypeName(t))
		}
		return nil
	})
	return threats, err
}

func decodeThreatURL(b []byte) ([]uint64, error) {
	var types []uint64
	err := forEachField(b, func(num protowire.Number, typ protowire.Type, v []byte, n uint64) error {
		if num != 2 {
			return nil
		}
		switch typ {
		case protowire.VarintType: // unpacked
			types = append(types, n)
		case protowire.BytesType: // packed
			for len(v) > 0 {
				x, l := protowire.ConsumeVarint(v)
				if l < 0 {
					return protowire.ParseError(l)
				}
				types = append(types, x)
				v = v[l:]
			}
		default:
			return errors.New("threat_types: unexpected wire type")
		}
		return nil
	})
	return types, err
}

// forEachField walks the top-level fields of a protobuf message. For bytes
// fields v is the payload; for varint fields n is the value.
func forEachField(b []byte, fn func(num protowire.Number, typ protowire.Type, v []byte, n uint64) error) error {
	for len(b) > 0 {
		num, typ, l := protowire.ConsumeTag(b)
		if l < 0 {
			return protowire.ParseError(l)
		}
		b = b[l:]
		var (
			v []byte
			n uint64
		)
		switch typ {
		case protowire.VarintType:
			n, l = protowire.ConsumeVarint(b)
		case protowire.BytesType:
			v, l = protowire.ConsumeBytes(b)
		default:
			l = protowire.ConsumeFieldValue(num, typ, b)
		}
		if l < 0 {
			return protowire.ParseError(l)
		}
		b = b[l:]
		if err := fn(num, typ, v, n); err != nil {
			return err
		}
	}
	return nil
}

// decodeError extracts the machine readable reason (ErrorInfo.reason, e.g.
// API_KEY_INVALID) and the message from a Google API error body, which is a
// google.rpc.Status in protobuf (default) or JSON.
//
//	message Status    { int32 code = 1; string message = 2; repeated google.protobuf.Any details = 3; }
//	message Any       { string type_url = 1; bytes value = 2; }
//	message ErrorInfo { string reason = 1; string domain = 2; ... }
func decodeError(contentType string, body []byte) (reason, message string) {
	mt, _, _ := mime.ParseMediaType(contentType)
	if mt == "application/json" || (len(body) > 0 && body[0] == '{') {
		return decodeJSONError(body)
	}
	reason = "unknown"
	_ = forEachField(body, func(num protowire.Number, typ protowire.Type, v []byte, n uint64) error {
		switch {
		case num == 2 && typ == protowire.BytesType:
			message = string(v)
		case num == 3 && typ == protowire.BytesType:
			var typeURL string
			var value []byte
			_ = forEachField(v, func(num protowire.Number, typ protowire.Type, v []byte, n uint64) error {
				if typ == protowire.BytesType {
					switch num {
					case 1:
						typeURL = string(v)
					case 2:
						value = v
					}
				}
				return nil
			})
			if strings.HasSuffix(typeURL, "google.rpc.ErrorInfo") && reason == "unknown" {
				_ = forEachField(value, func(num protowire.Number, typ protowire.Type, v []byte, n uint64) error {
					if num == 1 && typ == protowire.BytesType && len(v) > 0 {
						reason = string(v)
					}
					return nil
				})
			}
		}
		return nil
	})
	return reason, sanitizeMessage(message)
}

func decodeJSONError(body []byte) (reason, message string) {
	var e struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return "unknown", ""
	}
	message = sanitizeMessage(e.Error.Message)
	for _, d := range e.Error.Details {
		if d.Reason != "" {
			return d.Reason, message
		}
	}
	if e.Error.Status != "" {
		return e.Error.Status, message
	}
	return "unknown", message
}

func sanitizeMessage(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
