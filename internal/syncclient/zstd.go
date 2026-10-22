package syncclient

import (
	"errors"

	"github.com/klauspost/compress/zstd"
)

// singleFrame checks framing before decoding: DecodeAll otherwise accepts
// concatenated and skippable frames, including empty trailing frames.
func singleFrame(b []byte, size int) error {
	var h zstd.Header
	if err := h.Decode(b); err != nil {
		return err
	}
	if h.Skippable || h.DictionaryID != 0 || (h.HasFCS && h.FrameContentSize != uint64(size)) {
		return errors.New("invalid zstd frame header")
	}
	pos := h.HeaderSize
	for {
		if len(b)-pos < 3 {
			return errors.New("truncated zstd block")
		}
		v := uint32(b[pos]) | uint32(b[pos+1])<<8 | uint32(b[pos+2])<<16
		pos += 3
		n := int(v >> 3)
		switch (v >> 1) & 3 {
		case 1:
			n = 1 // RLE stores one byte, not the regenerated length.
		case 3:
			return errors.New("reserved zstd block type")
		}
		if n > len(b)-pos {
			return errors.New("truncated zstd block")
		}
		pos += n
		if v&1 != 0 {
			break
		}
	}
	if h.HasCheckSum {
		pos += 4
	}
	if pos != len(b) {
		return errors.New("zstd requires exactly one complete frame")
	}
	return nil
}

func decompress(b []byte, size int) ([]byte, error) {
	if size < 0 || size > 64<<20 {
		return nil, errors.New("invalid zstd size")
	}
	if err := singleFrame(b, size); err != nil {
		return nil, err
	}
	d, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20), zstd.WithDecoderMaxWindow(64<<20), zstd.WithDecodeAllCapLimit(true))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	out, err := d.DecodeAll(b, make([]byte, 0, size))
	if err != nil {
		return nil, err
	}
	if len(out) != size {
		return nil, errors.New("zstd size mismatch")
	}
	return out, nil
}

// A publication owns one encoder and releases it at the end. EncodeAll emits
// independent frames; reuse avoids rebuilding the match tables for each shard.
type blockCompressor struct{ encoder *zstd.Encoder }

func (c *blockCompressor) close() {
	if c.encoder != nil {
		c.encoder.Close()
		c.encoder = nil
	}
}

func (c *blockCompressor) compress(b []byte) ([]byte, error) {
	if c.encoder == nil {
		e, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)), zstd.WithZeroFrames(true))
		if err != nil {
			return nil, err
		}
		c.encoder = e
	}
	return c.encoder.EncodeAll(b, nil), nil
}

func compress(b []byte) ([]byte, error) {
	var c blockCompressor
	defer c.close()
	return c.compress(b)
}
