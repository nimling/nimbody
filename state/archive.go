package state

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"regexp"
	"strconv"

	"filippo.io/age"
	chunkers "github.com/PlakarKorp/go-cdc-chunkers"
	_ "github.com/PlakarKorp/go-cdc-chunkers/chunkers/fastcdc"
	"github.com/klauspost/compress/zstd"
)

const ArchiveMaxBytes = 1 << 30

const ChunkSeparator = ".part-"

const (
	ChunkFixed   = "fixed"
	ChunkFastCDC = "fastcdc"
)

var ErrKeyRequired = errors.New("the archive is encrypted and takes a key")

var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

var ageMagic = []byte("age-encryption.org/v1\n")

var chunkName = regexp.MustCompile(`^(.+)` + regexp.QuoteMeta(ChunkSeparator) + `([0-9]{4,})$`)

type ArchiveFile struct {
	Name string
	Data []byte
}

func IsEncrypted(data []byte) bool {
	return bytes.HasPrefix(data, ageMagic)
}

func IsArchive(data []byte) bool {
	return IsEncrypted(data) || bytes.HasPrefix(data, zstdMagic)
}

func Pack(files []ArchiveFile, key string) ([]byte, error) {
	if len(files) == 0 {
		return nil, errors.New("an archive holds at least one file")
	}
	var out bytes.Buffer
	var sink io.Writer = &out
	var sealed io.WriteCloser
	if key != "" {
		recipient, err := age.NewScryptRecipient(key)
		if err != nil {
			return nil, err
		}
		sealed, err = age.Encrypt(&out, recipient)
		if err != nil {
			return nil, err
		}
		sink = sealed
	}
	compressed, err := zstd.NewWriter(sink)
	if err != nil {
		return nil, err
	}
	archive := tar.NewWriter(compressed)
	for _, file := range files {
		if file.Name == "" {
			return nil, errors.New("an archive file carries a name")
		}
		if err := archive.WriteHeader(&tar.Header{Name: file.Name, Mode: 0o600, Size: int64(len(file.Data)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := archive.Write(file.Data); err != nil {
			return nil, err
		}
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	if err := compressed.Close(); err != nil {
		return nil, err
	}
	if sealed != nil {
		if err := sealed.Close(); err != nil {
			return nil, err
		}
	}
	return out.Bytes(), nil
}

func Unpack(data []byte, key string) ([]ArchiveFile, error) {
	var source io.Reader = bytes.NewReader(data)
	if IsEncrypted(data) {
		if key == "" {
			return nil, ErrKeyRequired
		}
		identity, err := age.NewScryptIdentity(key)
		if err != nil {
			return nil, err
		}
		opened, err := age.Decrypt(source, identity)
		if err != nil {
			var wrong *age.NoIdentityMatchError
			if errors.As(err, &wrong) {
				return nil, errors.New("the key does not open the archive")
			}
			return nil, fmt.Errorf("the archive does not decrypt: %w", err)
		}
		plain, err := io.ReadAll(io.LimitReader(opened, ArchiveMaxBytes+1))
		if err != nil {
			return nil, fmt.Errorf("the archive does not decrypt: %w", err)
		}
		if len(plain) > ArchiveMaxBytes {
			return nil, errors.New("the archive is larger than " + strconv.Itoa(ArchiveMaxBytes>>20) + " MiB")
		}
		data = plain
		source = bytes.NewReader(data)
	}
	if !bytes.HasPrefix(data, zstdMagic) {
		return nil, errors.New("the input is not a zstd compressed archive")
	}
	decompressed, err := zstd.NewReader(source)
	if err != nil {
		return nil, err
	}
	defer decompressed.Close()
	archive := tar.NewReader(io.LimitReader(decompressed, ArchiveMaxBytes+1))
	var files []ArchiveFile
	total := 0
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("the archive is not a tar: %w", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(archive)
		if err != nil {
			return nil, fmt.Errorf("the archive file %s is cut short: %w", header.Name, err)
		}
		total += len(body)
		if total > ArchiveMaxBytes {
			return nil, errors.New("the archive unpacks to more than " + strconv.Itoa(ArchiveMaxBytes>>20) + " MiB")
		}
		files = append(files, ArchiveFile{Name: header.Name, Data: body})
	}
	if len(files) == 0 {
		return nil, errors.New("the archive holds no files")
	}
	return files, nil
}

func Split(data []byte, size int, mode string) ([][]byte, error) {
	switch mode {
	case ChunkFixed, "":
	case ChunkFastCDC:
		if size == 0 || (size > 0 && len(data) <= size) {
			return [][]byte{data}, nil
		}
		return splitFastCDC(data, size)
	default:
		return nil, fmt.Errorf("the chunk mode %q is not %s or %s", mode, ChunkFixed, ChunkFastCDC)
	}
	if size <= 0 || len(data) <= size {
		return [][]byte{data}, nil
	}
	parts := make([][]byte, 0, len(data)/size+1)
	for len(data) > size {
		parts = append(parts, data[:size])
		data = data[size:]
	}
	return append(parts, data), nil
}

func splitFastCDC(data []byte, size int) ([][]byte, error) {
	standard, err := chunkers.NewChunker("fastcdc", bytes.NewReader(nil), nil)
	if err != nil {
		return nil, err
	}
	opts := &chunkers.ChunkerOpts{MinSize: standard.MinSize(), NormalSize: standard.NormalSize(), MaxSize: standard.MaxSize()}
	if size > 0 {
		opts.NormalSize = 1 << (bits.Len(uint(size)) - 1)
		opts.MinSize = opts.NormalSize / (standard.NormalSize() / standard.MinSize())
		opts.MaxSize = opts.NormalSize * (standard.MaxSize() / standard.NormalSize())
		if opts.MinSize < 64 {
			return nil, fmt.Errorf("the fastcdc chunk size is at least %d bytes", 64*standard.NormalSize()/standard.MinSize())
		}
	}
	chunker, err := chunkers.NewChunker("fastcdc", bytes.NewReader(data), opts)
	if err != nil {
		return nil, err
	}
	var parts [][]byte
	for {
		chunk, err := chunker.Next()
		if len(chunk) > 0 {
			parts = append(parts, append([]byte(nil), chunk...))
		}
		if errors.Is(err, io.EOF) {
			return parts, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func GetChunkName(base string, number int) string {
	return fmt.Sprintf("%s%s%04d", base, ChunkSeparator, number)
}

func ParseChunkName(name string) (string, int, bool) {
	match := chunkName.FindStringSubmatch(name)
	if match == nil {
		return "", 0, false
	}
	number, err := strconv.Atoi(match[2])
	if err != nil || number < 1 {
		return "", 0, false
	}
	return match[1], number, true
}
