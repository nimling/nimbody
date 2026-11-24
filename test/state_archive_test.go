package test

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
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
	parts := state.Split(data, 4)
	assert.Equal(t, [][]byte{[]byte("0123"), []byte("4567"), []byte("89")}, parts)
	assert.Equal(t, [][]byte{data}, state.Split(data, 0))
	assert.Equal(t, [][]byte{data}, state.Split(data, 10))

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
