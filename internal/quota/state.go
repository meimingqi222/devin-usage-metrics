package quota

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/egoist/mygo/plugins/sqlite"
)

func readStateValue(path, key string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	db, err := sqlite.Open(ctx, path, sqlite.OpenOptions{ReadOnly: true})
	if err != nil {
		return "", err
	}
	defer db.Close()
	rows, err := db.Query(ctx, "SELECT value FROM ItemTable WHERE key = ?", key)
	if err != nil {
		return "", err
	}
	if len(rows.Values) != 1 || len(rows.Values[0]) != 1 {
		return "", errors.New("state value unavailable")
	}
	s, ok := rows.Values[0][0].(string)
	if !ok {
		return "", errors.New("state value is not text")
	}
	return s, nil
}
func readAntigravityStateEmail(path string) string {
	raw, err := readStateValue(path, "antigravityAuthStatus")
	if err != nil {
		return ""
	}
	var d object
	_ = json.Unmarshal([]byte(raw), &d)
	return str(d["email"])
}

// Decode repeated protobuf fields without assuming the sentinel is the first entry.
func stateFields(b []byte, target uint64) (out [][]byte) {
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 || tag>>3 == 0 {
			return nil
		}
		b = b[n:]
		switch tag & 7 {
		case 0:
			_, n = binary.Uvarint(b)
			if n <= 0 {
				return nil
			}
			b = b[n:]
		case 1:
			if len(b) < 8 {
				return nil
			}
			b = b[8:]
		case 5:
			if len(b) < 4 {
				return nil
			}
			b = b[4:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || l > uint64(len(b)-n) {
				return nil
			}
			b = b[n:]
			if tag>>3 == target {
				out = append(out, b[:int(l)])
			}
			b = b[int(l):]
		default:
			return nil
		}
	}
	return out
}
func stateFirst(b []byte, field uint64) []byte {
	v := stateFields(b, field)
	if len(v) == 0 {
		return nil
	}
	return v[0]
}
func readAntigravityStateToken(path string) (object, error) {
	raw, err := readStateValue(path, "antigravityUnifiedStateSync.oauthToken")
	if err != nil {
		return nil, err
	}
	state, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	for _, entry := range stateFields(state, 1) {
		if string(stateFirst(entry, 1)) != "oauthTokenInfoSentinelKey" {
			continue
		}
		encoded := stateFirst(stateFirst(entry, 2), 1)
		info, err := base64.StdEncoding.DecodeString(string(encoded))
		if err != nil {
			continue
		}
		access, refresh := stateFirst(info, 1), stateFirst(info, 3)
		if len(access) > 0 && utf8.Valid(access) && utf8.Valid(refresh) {
			return object{"access_token": string(access), "refresh_token": string(refresh)}, nil
		}
	}
	return nil, errors.New("Antigravity current login token unavailable; reopen Antigravity and sign in")
}
