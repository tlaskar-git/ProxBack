package engine

import (
	"bytes"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Chunk compression modes, as stored in the settings and passed in Options.
const (
	// CompressionZstd compresses every chunk with zstd after chunking.
	CompressionZstd = "zstd"
	// CompressionOff stores chunks verbatim.
	CompressionOff = "off"
)

// chunkMagic prefixes the stored form of a compressed chunk. Readers sniff it:
// an object that does not start with it is raw chunk data, which is what every
// chunk written before v0.3.2 is. Compression therefore needs no manifest flag
// and no migration — old and new chunks coexist in the same restore point.
var chunkMagic = []byte("PBZ1")

// Compression happens after chunking, never before, and the chunk's identity
// (its SHA-256, and therefore its S3 key and its dedup index row) is always the
// hash of the RAW bytes. That is what keeps chunk boundaries stable: compressing
// the stream first would shift every later chunk after a single early edit and
// turn every incremental into a near-full.

var (
	encoderOnce sync.Once
	encoder     *zstd.Encoder
	decoderOnce sync.Once
	decoder     *zstd.Decoder
)

// sharedEncoder returns the process-wide zstd encoder, or nil when one could not
// be built (in which case chunks are stored raw). EncodeAll is goroutine-safe,
// so one encoder serves every engine and every upload worker; building one per
// chunk would cost more than the compression itself.
func sharedEncoder() *zstd.Encoder {
	encoderOnce.Do(func() {
		enc, err := zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.SpeedDefault),
			// Chunks are at most ChunkSize, so a larger window would only waste
			// memory — and it caps what a decoder has to allocate.
			zstd.WithWindowSize(ChunkSize),
			// The SHA-256 verification on restore is the integrity gate; a zstd
			// checksum on top of it would only cost bytes.
			zstd.WithEncoderCRC(false),
		)
		if err != nil {
			return
		}
		encoder = enc
	})
	return encoder
}

// sharedDecoder returns the process-wide zstd decoder, or nil when one could not
// be built. DecodeAll is goroutine-safe.
func sharedDecoder() *zstd.Decoder {
	decoderOnce.Do(func() {
		dec, err := zstd.NewReader(nil,
			// Raw chunks never exceed ChunkSize; the bound turns a corrupt frame
			// header into an error instead of a huge allocation.
			zstd.WithDecoderMaxMemory(4*ChunkSize),
		)
		if err != nil {
			return
		}
		decoder = dec
	})
	return decoder
}

// compressChunk returns the form a raw chunk should be stored in: the PBZ1 magic
// followed by a zstd frame, or raw when compression does not pay — already
// compressed or encrypted data would otherwise grow. The caller can tell which
// it got by comparing lengths; the reader tells by the magic.
func compressChunk(raw []byte) []byte {
	enc := sharedEncoder()
	if enc == nil {
		return raw
	}
	out := make([]byte, 0, len(raw)+len(chunkMagic))
	out = append(out, chunkMagic...)
	out = enc.EncodeAll(raw, out)
	if len(out) >= len(raw) {
		return raw
	}
	return out
}

// decodeChunk turns a stored chunk object back into raw chunk bytes. Objects
// without the magic are raw by definition. A decompression failure on an object
// that does have the magic means it is really raw data that happens to start
// with those four bytes (or a damaged object): fall back to raw and let the
// SHA-256 verification after reassembly decide, exactly as it does for every
// other chunk.
func decodeChunk(stored []byte) []byte {
	if !bytes.HasPrefix(stored, chunkMagic) {
		return stored
	}
	dec := sharedDecoder()
	if dec == nil {
		return stored
	}
	out, err := dec.DecodeAll(stored[len(chunkMagic):], nil)
	if err != nil {
		return stored
	}
	return out
}
