package quota

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"time"
	"unicode/utf8"

	"github.com/meimingqi222/devin-usage-metrics/internal/sqlite"
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
			doc := object{"access_token": string(access), "refresh_token": string(refresh)}
			// The state database also names the account; keep it with the
			// token so profiles can tell two Antigravity accounts apart.
			if email := readAntigravityStateEmail(path); email != "" {
				doc["email"] = email
			}
			return doc, nil
		}
	}
	return nil, errors.New("Antigravity current login token unavailable; reopen Antigravity and sign in")
}

// WriteAntigravityStateToken replaces the oauth token entry in Antigravity's
// state database, encoded exactly as the client itself writes it. The client
// keeps an in-memory copy and writes it back on shutdown, so the switch path
// quits it first; this only puts the credential in the database.
func WriteAntigravityStateToken(path string, doc object) error {
	tok := antigravityTokenEntry(doc)
	if tok == "" {
		return errors.New("credential has no Antigravity token fields")
	}
	if _, err := os.Stat(path); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := sqlite.Open(ctx, path, sqlite.OpenOptions{})
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err = db.Execute(ctx, "PRAGMA busy_timeout=2000"); err != nil {
		return err
	}
	_, err = db.Execute(ctx, "INSERT OR REPLACE INTO ItemTable(key,value) VALUES(?,?)", "antigravityUnifiedStateSync.oauthToken", tok)
	return err
}

// antigravityTokenEntry encodes a credential document as the state database's
// oauth token row: a base64 protobuf holding the opaque tokens beside a
// timestamp and the id token.
func antigravityTokenEntry(doc object) string {
	token := obj(doc["token"])
	if token == nil {
		token = doc
	}
	access := str(token["access_token"])
	refresh := str(token["refresh_token"])
	idToken := str(doc["id_token"])
	if idToken == "" {
		idToken = str(token["id_token"])
	}
	if access == "" || refresh == "" {
		return ""
	}
	seconds := int64(0)
	if exp, ok := stateExpiry(token["expiry"]); ok {
		seconds = exp.Unix()
	} else if exp, ok := stateExpiry(doc["expiry_date"]); ok {
		seconds = exp.Unix()
	}
	timestamp := binary.AppendUvarint(nil, uint64(1<<3|0))
	timestamp = binary.AppendUvarint(timestamp, uint64(max(0, seconds)))
	timestamp = binary.AppendUvarint(timestamp, uint64(2<<3|0))
	timestamp = binary.AppendUvarint(timestamp, 0)
	info := pbStateString(1, access)
	info = append(info, pbStateString(2, "Bearer")...)
	info = append(info, pbStateString(3, refresh)...)
	info = append(info, pbStateLen(4, timestamp)...)
	if idToken != "" {
		info = append(info, pbStateString(5, idToken)...)
	}
	row := pbStateString(1, base64.StdEncoding.EncodeToString(info))
	entry := append(pbStateString(1, "oauthTokenInfoSentinelKey"), pbStateLen(2, row)...)
	topic := pbStateLen(1, entry)
	return base64.StdEncoding.EncodeToString(topic)
}

func stateExpiry(v any) (time.Time, bool) {
	switch t := v.(type) {
	case string:
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
			if parsed, err := time.Parse(layout, t); err == nil {
				return parsed, true
			}
		}
	case float64:
		if t > 1e12 {
			return time.UnixMilli(int64(t)), true
		}
		return time.Unix(int64(t), 0), true
	case int64:
		if t > 1e12 {
			return time.UnixMilli(t), true
		}
		return time.Unix(t, 0), true
	case int:
		return time.Unix(int64(t), 0), true
	}
	return time.Time{}, false
}

func pbStateString(field int, value string) []byte {
	return pbStateLen(field, []byte(value))
}

func pbStateLen(field int, value []byte) []byte {
	out := binary.AppendUvarint(nil, uint64(field<<3|2))
	out = binary.AppendUvarint(out, uint64(len(value)))
	return append(out, value...)
}
