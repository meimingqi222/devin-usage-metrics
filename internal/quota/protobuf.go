package quota

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

func pbBytes(field uint64, b []byte) []byte {
	out := binary.AppendUvarint(nil, field<<3|2)
	out = binary.AppendUvarint(out, uint64(len(b)))
	return append(out, b...)
}
func pbFind(b []byte, target uint64) ([]byte, uint64, bool) {
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 || tag>>3 == 0 {
			return nil, 0, false
		}
		b = b[n:]
		switch tag & 7 {
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, 0, false
			}
			if tag>>3 == target {
				return nil, v, true
			}
			b = b[n:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || l > uint64(len(b)-n) {
				return nil, 0, false
			}
			b = b[n:]
			if tag>>3 == target {
				return b[:int(l)], 0, true
			}
			b = b[int(l):]
		case 1:
			if len(b) < 8 {
				return nil, 0, false
			}
			b = b[8:]
		case 5:
			if len(b) < 4 {
				return nil, 0, false
			}
			b = b[4:]
		default:
			return nil, 0, false
		}
	}
	return nil, 0, false
}
func fetchCLI(ctx context.Context, token string) (Result, error) {
	meta := []byte{}
	for _, f := range []struct {
		n uint64
		s string
	}{{1, "chisel"}, {2, "3000.6.11"}, {3, token}, {7, "3000.6.11"}} {
		meta = append(meta, pbBytes(f.n, []byte(f.s))...)
	}
	b, e := request(ctx, "POST", "https://server.codeium.com/exa.seat_management_pb.SeatManagementService/GetUserStatus", string(pbBytes(1, meta)), map[string]string{"Authorization": "Basic " + token + "-" + token, "Content-Type": "application/proto", "connect-protocol-version": "1", "Accept": "*/*"}, false)
	if e != nil {
		return Result{}, e
	}
	user, _, ok := pbFind(b, 1)
	if !ok {
		return Result{}, errors.New("GetUserStatus response missing F1")
	}
	quota, _, ok := pbFind(user, 13)
	if !ok {
		return Result{}, errors.New("GetUserStatus response missing quota")
	}
	r := Result{}
	plan, _, _ := pbFind(quota, 1)
	name, _, _ := pbFind(plan, 2)
	r.Plan = strings.ToLower(string(name))
	for _, f := range []struct {
		n, reset uint64
		label    string
	}{{14, 17, "Daily"}, {15, 18, "Weekly"}} {
		_, v, ok := pbFind(quota, f.n)
		if ok {
			_, reset, _ := pbFind(quota, f.reset)
			r.Windows = append(r.Windows, Window{f.label, 100 - float64(v), int64(reset)})
		}
	}
	_, over, _ := pbFind(quota, 16)
	if over > 0 {
		r.Extra = fmt.Sprintf("%.2f ACU", float64(over)/1e6)
	}
	if r.Plan == "" && len(r.Windows) == 0 && r.Extra == "" {
		return Result{}, errors.New("GetUserStatus returned no quota fields")
	}
	return r, nil
}
