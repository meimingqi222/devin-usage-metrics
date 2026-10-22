package syncclient

import (
	"bytes"
	"encoding/json"
	"testing"
)

func BenchmarkCompressPublicationBlock(b *testing.B) {
	raw, err := json.Marshal(sessionSortFixture(500))
	if err != nil {
		b.Fatal(err)
	}
	var c blockCompressor
	defer c.close()
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	for b.Loop() {
		z, err := c.compress(raw)
		if err != nil || len(z) == 0 {
			b.Fatal(err)
		}
	}
}

func TestCompressionRepeatedBlocksStayIndependent(t *testing.T) {
	var c blockCompressor
	defer c.close()
	for i := 0; i < 20; i++ {
		raw := bytes.Repeat([]byte{byte(i), 0, 255}, 1000+i)
		z, err := c.compress(raw)
		if err != nil {
			t.Fatal(err)
		}
		fresh, e := compress(raw)
		if e != nil || !bytes.Equal(fresh, z) {
			t.Fatalf("reused frame differs from fresh encoder: %v", e)
		}
		got, err := decompress(z, len(raw))
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("block %d mismatch: %v", i, err)
		}
	}
}
