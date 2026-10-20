package syncclient

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestIndependentZstdFixture(t *testing.T) {
	// Reference libzstd CLI v1.5.4, not this Go encoder:
	// printf '%s' '{"protocol":3,"data_schema":1,"turns":[]}' | zstd -3 -q -c | base64 -w0
	// Streaming frame with no content-size field, plus XXH64 checksum.
	z, err := base64.StdEncoding.DecodeString("KLUv/QRYSQEAeyJwcm90b2NvbCI6MywiZGF0YV9zY2hlbWEiOjEsInR1cm5zIjpbXX3lL5Yd")
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(`{"protocol":3,"data_schema":1,"turns":[]}`)
	got, err := decompress(z, len(want))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("reference frame: %s, %v", got, err)
	}
	corrupt := append([]byte(nil), z...)
	corrupt[len(corrupt)-1] ^= 1
	empty, err := compress(nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, input := range map[string][]byte{
		"checksum":             corrupt,
		"truncated":            z[:len(z)-1],
		"concatenated":         append(append([]byte(nil), z...), z...),
		"trailing empty frame": append(append([]byte(nil), z...), empty...),
		"trailing garbage":     append(append([]byte(nil), z...), 0),
		"skippable":            {0x50, 0x2a, 0x4d, 0x18, 0, 0, 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decompress(input, len(want)); err == nil {
				t.Fatal("accepted invalid frame")
			}
		})
	}
	for _, size := range []int{-1, 0, len(want) - 1, len(want) + 1, 64<<20 + 1} {
		if _, err := decompress(z, size); err == nil {
			t.Fatalf("accepted declared size %d", size)
		}
	}
}

func TestZstdRoundTrip(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte("hello"), bytes.Repeat([]byte("abc"), 100000)} {
		z, err := compress(raw)
		if err != nil {
			t.Fatal(err)
		}
		out, err := decompress(z, len(raw))
		if err != nil || !bytes.Equal(out, raw) {
			t.Fatalf("roundtrip length %d: %v", len(raw), err)
		}
	}
}

func TestBlockRefIncludesZeroTurnCount(t *testing.T) {
	b, err := json.Marshal(reference{File: "v3-block-example.zst"})
	if err != nil || !bytes.Contains(b, []byte(`"turn_count":0`)) {
		t.Fatalf("Rust BlockRef requires turn_count: %s %v", b, err)
	}
}
