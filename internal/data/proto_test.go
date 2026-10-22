package data

import (
	"encoding/binary"
	"reflect"
	"testing"
)

func referenceProto(b []byte) []field {
	var out []field
	for len(b) > 0 {
		k, n := binary.Uvarint(b)
		if n <= 0 {
			return out
		}
		b = b[n:]
		f := field{n: k >> 3}
		switch k & 7 {
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return out
			}
			f.v = v
			b = b[n:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || l > uint64(len(b)-n) {
				return out
			}
			f.b = b[n : n+int(l)]
			b = b[n+int(l):]
		case 1:
			if len(b) < 8 {
				return out
			}
			b = b[8:]
		case 5:
			if len(b) < 4 {
				return out
			}
			b = b[4:]
		default:
			return out
		}
		out = append(out, f)
	}
	return out
}
func referencePB(b []byte, path ...uint64) field {
	for i, n := range path {
		var found field
		for _, f := range referenceProto(b) {
			if f.n == n {
				found = f
				break
			}
		}
		if i == len(path)-1 {
			return found
		}
		b = found.b
	}
	return field{}
}
func FuzzProtoLookup(f *testing.F) {
	f.Add([]byte{8, 1, 8, 2, 18, 2, 8, 3}, uint8(1))
	f.Add([]byte{8, 1, 255}, uint8(1))
	f.Add([]byte{74, 2, 8, 4}, uint8(9))
	f.Fuzz(func(t *testing.T, raw []byte, key uint8) {
		for _, path := range [][]uint64{{uint64(key)}, {uint64(key), 1}, {9, 2}, {1, 19}} {
			if !reflect.DeepEqual(pb(raw, path...), referencePB(raw, path...)) {
				t.Fatal("protobuf lookup changed")
			}
		}
	})
}
func TestProtoLookupDoesNotAllocate(t *testing.T) {
	raw := []byte{8, 1, 16, 2, 24, 3, 32, 4, 40, 5}
	if n := testing.AllocsPerRun(100, func() {
		if pb(raw, 5).v != 5 {
			t.Fatal("missing value")
		}
	}); n != 0 {
		t.Fatalf("lookup allocated %g objects", n)
	}
}
