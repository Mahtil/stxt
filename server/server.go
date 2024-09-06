package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"cdr.dev/slog"
	"cloud.google.com/go/storage"
	"github.com/go-chi/chi/v5"
	"github.com/gorilla/schema"
	"google.golang.org/api/googleapi"
)

type Server struct {
	Log     slog.Logger
	Storage *storage.Client
}

//go:embed all:dist/**
var staticFS embed.FS

const maxNoteSize = 100 << 20

func (s *Server) Handler() http.Handler {
	subfs, err := fs.Sub(staticFS, "dist")
	if err != nil {
		panic(err)
	}

	r := chi.NewRouter()
	r.Use(func(h http.Handler) http.Handler {
		return http.MaxBytesHandler(h, maxNoteSize*2)
	})

	fileServer := http.FileServer(http.FS(subfs))
	// r.Handle("/*", fileServer)

	// Oh man is this janky.
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/about":
			r.URL.Path = "/about.html"
		case len(r.URL.Path) > 1 && strings.Count(r.URL.Path, "/") == 1 && !strings.Contains(r.URL.Path, "."):
			// Redirect "view" directives to the index.html.
			w.Header().Set("Redirected-From", r.URL.Path)
			r.URL.Path = "[...path].html"
		}
		fileServer.ServeHTTP(w, r)
	})

	r.Route("/api", func(r chi.Router) {
		r.Post("/notes", s.postNote)
		r.Get("/notes/{id}", s.getNote)
	})

	return r
}

type note struct {
	Contents         string    `schema:"contents" json:"contents"`
	ExpiresAt        time.Time `schema:"expires_at" json:"expires_at"`
	DestroyAfterRead bool      `schema:"destroy_after_read" json:"destroy_after_read"`
}

var decoder = schema.NewDecoder()

func writePlainText(w http.ResponseWriter, statusCode int, message string, as ...interface{}) {
	w.WriteHeader(statusCode)
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, message, as...)
}

func writeJSON(w http.ResponseWriter, statusCode int, message interface{}) {
	w.WriteHeader(statusCode)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(message)
}

const bucketName = "scr-notes"

const noteNameCharset = "abcdefghijklmnopqrstuvwxyz0123456789"

func init() {
	rand.Seed(time.Now().UnixNano())
}

func randNoteChar() byte {
	return noteNameCharset[rand.Intn(len(noteNameCharset))]
}

// findObjectID finds a short ID to use for a new object note.
// It's OK if there is a conflict due to parallel upload, that's why
// we use an If condition.
func (s *Server) findObjectID(ctx context.Context) (string, error) {
	bucket := s.Storage.Bucket(bucketName)

	var name string
	for {
		// Minimum name of 4 characters
		name += string(randNoteChar())
		if len(name) < 4 {
			continue
		}

		obj := bucket.Object(name)
		_, err := obj.Attrs(ctx)
		if err != nil {
			if err == storage.ErrObjectNotExist {
				return name, nil
			}
			return "", err
		}
	}
}

func (s *Server) getNote(w http.ResponseWriter, r *http.Request) {
	objectID := chi.URLParam(r, "id")

	w.Header().Set("Cache-Control", "no-cache")

	reader, err := s.Storage.Bucket(bucketName).Object(objectID).NewReader(r.Context())
	if err != nil {
		if err == storage.ErrObjectNotExist {
			writePlainText(w, http.StatusNotFound, "Object doesn't exist")
			return
		}
		writePlainText(w, http.StatusInternalServerError, "Something ain't right")
		return
	}
	defer reader.Close()

	var n note
	err = json.NewDecoder(reader).Decode(&n)
	if err != nil {
		writePlainText(w, http.StatusInternalServerError, "note corrupt: %v", err)
		return
	}

	// When peek is set, we don't return the contents but we let the viewer
	// check the metadata. This is useful for prompting an "are you sure you want
	// to see the note?"
	if r.URL.Query().Has("peek") && n.DestroyAfterRead {
		n.Contents = ""
		writeJSON(w, http.StatusOK, n)
		return
	}

	isExpired := n.ExpiresAt.Before(time.Now())
	if n.DestroyAfterRead || isExpired {
		err = s.Storage.Bucket(bucketName).Object(objectID).Delete(context.Background())
		if err != nil {
			s.Log.Error(
				r.Context(), "destroy note",
				slog.F("id", objectID), slog.Error(err),
			)
			if err == storage.ErrObjectNotExist {
				// I'm not sure why this happens, but it does, and the object
				// is always actually still deleted. Let's just 404.
				writePlainText(w, http.StatusNotFound, "note not found")
				return
			}
		}
	}

	if isExpired {
		// sshhh
		writePlainText(w, http.StatusNotFound, "note not found")
		return
	}

	writeJSON(w, http.StatusOK, n)
}

type limitErrReader struct {
	io.Reader
	limit int64
}

func (l *limitErrReader) Read(p []byte) (n int, err error) {
	n, err = l.Reader.Read(p)
	l.limit -= int64(n)
	if l.limit < 0 {
		return 0, errors.New("limit exceeded")
	}
	return
}

func (s *Server) postNote(w http.ResponseWriter, r *http.Request) {
	err := r.ParseMultipartForm(maxNoteSize)
	if err != nil {
		writePlainText(w, http.StatusBadRequest, "parse form data: %v", err)
		return
	}

	var parsedReq note
	err = decoder.Decode(&parsedReq, r.PostForm)
	if err != nil {
		writePlainText(w, http.StatusBadRequest, "decode form data: %v", err)
		return
	}
	if len(parsedReq.Contents) > maxNoteSize {
		writePlainText(w, http.StatusBadRequest, "Contents exceed max note size of %v bytes", maxNoteSize)
		return
	}
	if parsedReq.ExpiresAt.After(time.Now().AddDate(0, 0, 30)) {
		writePlainText(w, http.StatusBadRequest, "Note expires too far into the future")
		return
	}

	// Get the contents from the form data
	file, _, err := r.FormFile("contents")
	if err != nil {
		writePlainText(w, http.StatusBadRequest, "get form file: %v", err)
		return
	}
	defer file.Close()

	limitedReader := &limitErrReader{Reader: file, limit: maxNoteSize}
	var buf bytes.Buffer
	_, err = io.Copy(&buf, limitedReader)
	if err != nil {
		writePlainText(w, http.StatusBadRequest, "Contents exceed max note size of %v bytes", maxNoteSize)
		return
	}
	parsedReq.Contents = buf.String()

	for attempts := 0; attempts < 10; attempts++ {
		objectName, err := s.findObjectID(r.Context())
		if err != nil {
			writePlainText(w, http.StatusInternalServerError, "An internal error occured")
			s.Log.Error(r.Context(), "find object id", slog.Error(err))
			return
		}

		objectHandle := s.Storage.Bucket(bucketName).Object(objectName).If(storage.Conditions{
			// Very important!
			// Conflicts are common and possible due to our ID generation logic.
			DoesNotExist: true,
		})

		// There is a lot of copying and amplification due to JSON. I'll fix it
		// if the service becomes popular. But, for now, I don't want to deal
		// with the work.

		hw := objectHandle.NewWriter(r.Context())
		json.NewEncoder(hw).Encode(parsedReq)
		err = hw.Close()
		if err != nil {
			if e, ok := err.(*googleapi.Error); ok {
				// There was a race condition for the object name.
				if e.Code == http.StatusPreconditionFailed {
					hw.Close()
					continue
				}
			}
			writePlainText(w, http.StatusInternalServerError, "failed to write")
			s.Log.Error(r.Context(), "write to storage", slog.Error(err))
			return
		}
		csm := sha256.Sum256([]byte(parsedReq.Contents))
		s.Log.Info(r.Context(), "created note", slog.F("id", objectName),
			slog.F("size", len(parsedReq.Contents)),
			slog.F("checksum", hex.EncodeToString(csm[:])),
		)
		writePlainText(w, http.StatusCreated, objectName)
		return
	}

	writePlainText(w, http.StatusInternalServerError, "An internal error occured")
	s.Log.Error(r.Context(), "could not allocate object id in time")
}
