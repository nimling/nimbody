package test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	mathrand "math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/nimling/nimbody/cli"
	"github.com/nimling/nimbody/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStatePackRoundTripsFilesPlainAndEncrypted(t *testing.T) {
	files := []state.ArchiveFile{
		{Name: "one.json", Data: []byte(`[{"a":1}]`)},
		{Name: "docs/two.md", Data: []byte("---\nkey: x\n---\nbody\n")},
		{Name: "three.ndjson", Data: []byte("{}\n{}\n")},
	}
	for _, key := range []string{"", "any string at all, with spaces and ünïcode"} {
		packed, err := state.Pack(files, key)
		require.NoError(t, err)
		assert.True(t, state.IsArchive(packed))
		assert.Equal(t, key != "", state.IsEncrypted(packed))
		opened, err := state.Unpack(packed, key)
		require.NoError(t, err)
		assert.Equal(t, files, opened)
	}

	sealed, err := state.Pack(files, "secret")
	require.NoError(t, err)
	_, err = state.Unpack(sealed, "")
	assert.ErrorIs(t, err, state.ErrKeyRequired)
	_, err = state.Unpack(sealed, "other")
	assert.ErrorContains(t, err, "the key does not open the archive")
	_, err = state.Unpack([]byte("plain text"), "")
	assert.Error(t, err)
	_, err = state.Pack(nil, "")
	assert.Error(t, err)
}

func TestStateSplitAndChunkNames(t *testing.T) {
	data := []byte("0123456789")
	parts, err := state.Split(data, 4, state.ChunkFixed)
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("0123"), []byte("4567"), []byte("89")}, parts)
	for _, size := range []int{0, 10} {
		whole, err := state.Split(data, size, state.ChunkFixed)
		require.NoError(t, err)
		assert.Equal(t, [][]byte{data}, whole)
	}
	_, err = state.Split(data, 4, "rabin")
	assert.Error(t, err)

	name := state.GetChunkName("out.tar.zst", 3)
	assert.Equal(t, "out.tar.zst.part-0003", name)
	base, number, ok := state.ParseChunkName(name)
	assert.True(t, ok)
	assert.Equal(t, "out.tar.zst", base)
	assert.Equal(t, 3, number)
	_, _, ok = state.ParseChunkName("out.tar.zst")
	assert.False(t, ok)
}

func randomAreas(t *testing.T, count int) *memory {
	m := newMemory("a")
	for i := 0; i < count; i++ {
		raw := make([]byte, 4<<10)
		_, err := rand.Read(raw)
		require.NoError(t, err)
		m.add("area", fmt.Sprintf("area-%04d", i), map[string]any{"name": hex.EncodeToString(raw)})
	}
	return m
}

func areaNames(m *memory) map[string]any {
	names := map[string]any{}
	for _, row := range m.rows["area"] {
		names[m.key(row)] = row.data["name"]
	}
	return names
}

func TestStateCommandRoundTripsAnEncryptedChunkedArchive(t *testing.T) {
	source := randomAreas(t, 800)
	t.Setenv("DEMO_STATE_KEY", "a key with spaces")
	var calls []string
	archive := filepath.Join(t.TempDir(), "areas.tar.zst.age")

	out, err := runState(stateSender(areaService(source), &calls), "export", "--archive", "-o", archive, "--chunk-size", "1")
	require.NoError(t, err, out)
	assert.Contains(t, out, "encrypted")
	assert.NoFileExists(t, archive, "a large result is split into chunk files")
	first, err := os.ReadFile(state.GetChunkName(archive, 1))
	require.NoError(t, err)
	assert.True(t, state.IsEncrypted(first), "the archive on disk is encrypted")
	assert.FileExists(t, state.GetChunkName(archive, 2))
	assert.FileExists(t, state.GetChunkName(archive, 3))

	chunks, err := filepath.Glob(archive + state.ChunkSeparator + "*")
	require.NoError(t, err)
	var whole []byte
	for i := range chunks {
		part, err := os.ReadFile(state.GetChunkName(archive, i+1))
		require.NoError(t, err)
		whole = append(whole, part...)
	}
	single := filepath.Join(t.TempDir(), "whole.tar.zst.age")
	require.NoError(t, os.WriteFile(single, whole, 0o600))

	for name, arg := range map[string]string{"the base name": archive, "a part": state.GetChunkName(archive, 2), "the whole file": single} {
		target := newMemory("b")
		calls = nil
		out, err = runState(stateSender(areaService(target), &calls), "import", arg)
		require.NoError(t, err, name+": "+out)
		assert.Equal(t, areaNames(source), areaNames(target), name)
	}

	folder := t.TempDir()
	for i := range chunks {
		part, err := os.ReadFile(state.GetChunkName(archive, i+1))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(state.GetChunkName(filepath.Join(folder, "areas.tar.zst.age"), i+1), part, 0o600))
	}
	target := newMemory("b")
	out, err = runState(stateSender(areaService(target), &calls), "import", folder)
	require.NoError(t, err, out)
	assert.Equal(t, areaNames(source), areaNames(target), "a folder holding a chunk set imports it once")

	t.Setenv("DEMO_STATE_KEY", "")
	_, err = runState(stateSender(areaService(newMemory("b")), &calls), "import", single)
	require.Error(t, err)
	var refused *cli.Error
	require.ErrorAs(t, err, &refused)
	assert.Contains(t, refused.Hint, "DEMO_STATE_KEY")
	t.Setenv("DEMO_STATE_KEY", "wrong")
	_, err = runState(stateSender(areaService(newMemory("b")), &calls), "import", single)
	assert.ErrorContains(t, err, "the key does not open the archive")

	require.NoError(t, os.Remove(state.GetChunkName(archive, 2)))
	t.Setenv("DEMO_STATE_KEY", "a key with spaces")
	_, err = runState(stateSender(areaService(newMemory("b")), &calls), "import", archive)
	assert.ErrorContains(t, err, "no part 2", "a gap in the chunk set is refused")
}

func TestStateCommandRoundTripsAPlainArchiveInOneFile(t *testing.T) {
	source := seeded()
	var calls []string
	e, _ := stateService(source)
	archive := filepath.Join(t.TempDir(), "state.tar.zst")

	out, err := runState(stateSender(e, &calls), "export", "--archive", "-o", archive)
	require.NoError(t, err, out)
	packed, err := os.ReadFile(archive)
	require.NoError(t, err)
	assert.False(t, state.IsEncrypted(packed))
	files, err := state.Unpack(packed, "")
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, "state.ndjson", files[0].Name)

	target := newMemory("b")
	targetService, _ := stateService(target)
	out, err = runState(stateSender(targetService, &calls), "import", archive)
	require.NoError(t, err, out)
	assert.Equal(t, 2, target.count("location"))
	assert.Equal(t, 2, target.count("room"))

	_, err = runState(stateSender(e, &calls), "export", "--archive")
	assert.Error(t, err, "an archive is written to a file")
}

func TestStateCommandImportsMarkdownFilesFromAnArchive(t *testing.T) {
	m := newMemory("a")
	files := []state.ArchiveFile{
		{Name: "v1.1.81.md", Data: []byte(releaseNote)},
		{Name: "v1.1.82.md", Data: []byte("---\nkey: v1.1.82\ntitle: Second\n---\nbody\n")},
	}
	packed, err := state.Pack(files, "k")
	require.NoError(t, err)
	archive := filepath.Join(t.TempDir(), "notes.tar.zst.age")
	require.NoError(t, os.WriteFile(archive, packed, 0o600))
	t.Setenv("DEMO_STATE_KEY", "k")
	var calls []string
	out, err := runState(stateSender(articleService(m), &calls), "import", archive, "--target", "article")
	require.NoError(t, err, out)
	assert.Equal(t, 2, m.count("article"))
	assert.Equal(t, []string{"POST /api/state/import text/markdown", "POST /api/state/import text/markdown"}, calls)
}

func TestStateFastCDCSplitJoinsBackAndKeepsUnchangedChunks(t *testing.T) {
	data := make([]byte, 4<<20)
	_, err := mathrand.New(mathrand.NewSource(7)).Read(data)
	require.NoError(t, err)

	parts, err := state.Split(data, 64<<10, state.ChunkFastCDC)
	require.NoError(t, err)
	require.Greater(t, len(parts), 8)
	assert.Equal(t, data, bytes.Join(parts, nil))
	for _, part := range parts[:len(parts)-1] {
		assert.GreaterOrEqual(t, len(part), 16<<10)
		assert.LessOrEqual(t, len(part), 512<<10)
	}

	again, err := state.Split(data, 64<<10, state.ChunkFastCDC)
	require.NoError(t, err)
	assert.Equal(t, parts, again, "the same data gives the same chunks")

	edited := append([]byte(nil), data...)
	edited[len(edited)/2] ^= 0xff
	changed, err := state.Split(edited, 64<<10, state.ChunkFastCDC)
	require.NoError(t, err)
	seen := map[string]bool{}
	for _, part := range parts {
		seen[string(part)] = true
	}
	shared := 0
	for _, part := range changed {
		if seen[string(part)] {
			shared++
		}
	}
	assert.GreaterOrEqual(t, shared, len(changed)-3, "one changed byte leaves the other chunks as they were")

	shifted, err := state.Split(append([]byte("inserted at the front"), data...), 64<<10, state.ChunkFastCDC)
	require.NoError(t, err)
	shared = 0
	for _, part := range shifted {
		if seen[string(part)] {
			shared++
		}
	}
	assert.GreaterOrEqual(t, shared, len(shifted)-3, "an insert at the front moves no chunk boundary behind it")

	fixed, err := state.Split(append([]byte("inserted at the front"), data...), 64<<10, state.ChunkFixed)
	require.NoError(t, err)
	shared = 0
	for _, part := range fixed {
		if seen[string(part)] {
			shared++
		}
	}
	assert.Zero(t, shared, "a fixed split shifts every chunk")

	standard, err := state.Split(data, -1, state.ChunkFastCDC)
	require.NoError(t, err)
	assert.Equal(t, data, bytes.Join(standard, nil))
	for _, part := range standard[:len(standard)-1] {
		assert.GreaterOrEqual(t, len(part), 2<<10)
		assert.LessOrEqual(t, len(part), 64<<10)
	}
	assert.Greater(t, len(standard), len(parts), "the standard sizes cut finer than 64 KiB")

	_, err = state.Split(data, 128, state.ChunkFastCDC)
	assert.Error(t, err)
}

func TestStateCommandRoundTripsAFastCDCChunkedArchive(t *testing.T) {
	source := randomAreas(t, 800)
	var calls []string
	archive := filepath.Join(t.TempDir(), "areas.tar.zst")

	out, err := runState(stateSender(areaService(source), &calls), "export", "--archive", "-o", archive, "--chunk-size", "1", "--chunk-mode", "fastcdc")
	require.NoError(t, err, out)
	assert.NoFileExists(t, archive)
	chunks, err := filepath.Glob(archive + state.ChunkSeparator + "*")
	require.NoError(t, err)
	require.Greater(t, len(chunks), 1)

	repeat := filepath.Join(t.TempDir(), "areas.tar.zst")
	_, err = runState(stateSender(areaService(source), &calls), "export", "--archive", "-o", repeat, "--chunk-size", "1", "--chunk-mode", "fastcdc")
	require.NoError(t, err)
	for i := range chunks {
		first, err := os.ReadFile(state.GetChunkName(archive, i+1))
		require.NoError(t, err)
		second, err := os.ReadFile(state.GetChunkName(repeat, i+1))
		require.NoError(t, err)
		assert.Equal(t, first, second, "an export of unchanged data writes the same chunks")
	}

	for name, arg := range map[string]string{"the base name": archive, "a part": state.GetChunkName(archive, 2)} {
		target := newMemory("b")
		out, err = runState(stateSender(areaService(target), &calls), "import", arg)
		require.NoError(t, err, name+": "+out)
		assert.Equal(t, areaNames(source), areaNames(target), name)
	}

	_, err = runState(stateSender(areaService(source), &calls), "export", "--archive", "-o", filepath.Join(t.TempDir(), "x.tar.zst"), "--chunk-mode", "rabin")
	assert.ErrorContains(t, err, "chunk mode")
}
