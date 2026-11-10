package state

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
)

const (
	SessionLifetime = time.Hour
	SessionMaxBytes = 512 << 20
)

type Session struct {
	ID        string    `json:"id"`
	Parts     int       `json:"parts"`
	Bytes     int       `json:"bytes"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Part struct {
	Session string `json:"session"`
	Part    int    `json:"part"`
	Lines   int    `json:"lines"`
	Bytes   int    `json:"bytes"`
}

type staged struct {
	parts   map[int][]byte
	bytes   int
	expires time.Time
}

type sessions struct {
	mu    sync.Mutex
	items map[string]*staged
}

func (s *sessions) get(id string) (*staged, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purge()
	item, ok := s.items[id]
	if !ok {
		return nil, echo.NewHTTPError(http.StatusNotFound, "no import session "+id)
	}
	return item, nil
}

func (s *sessions) purge() {
	now := time.Now()
	for id, item := range s.items {
		if now.After(item.expires) {
			delete(s.items, id)
		}
	}
}

func (s *sessions) summary(id string, item *staged) Session {
	return Session{ID: id, Parts: len(item.parts), Bytes: item.bytes, ExpiresAt: item.expires}
}

type sessionInput struct {
	ID string `param:"id" json:"id"`
}

type partInput struct {
	ID    string          `param:"id" json:"id"`
	Part  string          `param:"part" json:"part"`
	Lines json.RawMessage `body:"*" json:"lines,omitempty"`
}

type commitInput struct {
	ID string `param:"id" json:"id"`
	ImportRequest
}

func (s *service) openImportSession(ctx echo.Context, in struct{}) (Session, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return Session{}, err
	}
	id := hex.EncodeToString(raw)
	item := &staged{parts: map[int][]byte{}, expires: time.Now().Add(SessionLifetime)}
	s.sessions.mu.Lock()
	defer s.sessions.mu.Unlock()
	s.sessions.purge()
	s.sessions.items[id] = item
	return s.sessions.summary(id, item), nil
}

func (s *service) putImportSessionPart(ctx echo.Context, in partInput) (Part, error) {
	item, err := s.sessions.get(in.ID)
	if err != nil {
		return Part{}, err
	}
	number, err := strconv.Atoi(in.Part)
	if err != nil || number < 1 {
		return Part{}, echo.NewHTTPError(http.StatusBadRequest, "a part is numbered from 1")
	}
	body, err := io.ReadAll(ctx.Request().Body)
	if err != nil {
		return Part{}, echo.NewHTTPError(http.StatusBadRequest, "the part could not be read: "+err.Error())
	}
	lines := 0
	for _, line := range bytes.Split(body, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if !json.Valid(line) {
			return Part{}, echo.NewHTTPError(http.StatusBadRequest, "part "+strconv.Itoa(number)+" line "+strconv.Itoa(lines+1)+" is not json; a part holds whole state lines")
		}
		lines++
	}
	s.sessions.mu.Lock()
	defer s.sessions.mu.Unlock()
	previous := len(item.parts[number])
	if item.bytes-previous+len(body) > SessionMaxBytes {
		return Part{}, echo.NewHTTPError(http.StatusRequestEntityTooLarge, "an import session holds at most "+strconv.Itoa(SessionMaxBytes>>20)+" MiB")
	}
	item.parts[number] = body
	item.bytes += len(body) - previous
	item.expires = time.Now().Add(SessionLifetime)
	return Part{Session: in.ID, Part: number, Lines: lines, Bytes: len(body)}, nil
}

func (s *service) commitImportSession(ctx echo.Context, in commitInput) (ImportResponse, error) {
	item, err := s.sessions.get(in.ID)
	if err != nil {
		return ImportResponse{}, err
	}
	if in.AssetID != "" {
		return ImportResponse{}, echo.NewHTTPError(http.StatusBadRequest, "a commit imports the staged parts, not an asset")
	}
	s.sessions.mu.Lock()
	var body bytes.Buffer
	for number := 1; number <= len(item.parts); number++ {
		part, ok := item.parts[number]
		if !ok {
			s.sessions.mu.Unlock()
			return ImportResponse{}, echo.NewHTTPError(http.StatusConflict, "part "+strconv.Itoa(number)+" is missing; the parts are numbered 1 to "+strconv.Itoa(len(item.parts))+" without a gap")
		}
		body.Write(part)
		if !bytes.HasSuffix(part, []byte("\n")) {
			body.WriteByte('\n')
		}
	}
	s.sessions.mu.Unlock()
	file, err := Read(&body)
	if err != nil {
		return ImportResponse{}, echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	out, err := s.importFile(s.context(ctx), in.ImportRequest, file)
	if err != nil {
		return ImportResponse{}, err
	}
	if !in.DryRun {
		s.sessions.mu.Lock()
		delete(s.sessions.items, in.ID)
		s.sessions.mu.Unlock()
	}
	return out, nil
}

func (s *service) deleteImportSession(ctx echo.Context, in sessionInput) (Session, error) {
	item, err := s.sessions.get(in.ID)
	if err != nil {
		return Session{}, err
	}
	s.sessions.mu.Lock()
	defer s.sessions.mu.Unlock()
	summary := s.sessions.summary(in.ID, item)
	delete(s.sessions.items, in.ID)
	return summary, nil
}
