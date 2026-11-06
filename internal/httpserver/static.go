package httpserver

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (s *Server) asset(w http.ResponseWriter, r *http.Request, name string, cache bool) {
	ServeAsset(w, r, s.config.StaticDir, name, cache)
}
func ServeAsset(w http.ResponseWriter, r *http.Request, directory, name string, cache bool) {
	if strings.EqualFold(filepath.Base(name), "telegram.html") || strings.EqualFold(filepath.Base(name), "telegram.js") {
		detail(w, 404, "Not Found")
		return
	}
	root, err := filepath.EvalSymlinks(directory)
	if err != nil {
		detail(w, 404, "Not Found")
		return
	}
	root, err = filepath.Abs(root)
	if err != nil {
		detail(w, 500, "Internal Server Error")
		return
	}
	path, err := filepath.EvalSymlinks(filepath.Join(root, name))
	if err != nil {
		detail(w, 404, "Not Found")
		return
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		detail(w, 404, "Not Found")
		return
	}
	file, err := os.Open(path)
	if err != nil {
		detail(w, 404, "Not Found")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		detail(w, 404, "Not Found")
		return
	}
	stamp := strconv.FormatFloat(float64(info.ModTime().UnixNano())/1e9, 'f', -1, 64)
	if !strings.Contains(stamp, ".") {
		stamp += ".0"
	}
	sum := md5.Sum([]byte(stamp + "-" + strconv.FormatInt(info.Size(), 10)))
	tag := "\"" + hex.EncodeToString(sum[:]) + "\""
	w.Header().Set("ETag", tag)
	if cache {
		w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
	}
	if info.Size() <= 2<<20 && (info.Name() == "panel.js" || info.Name() == "panel.html" || info.Name() == "aaid.js") {
		body, err := io.ReadAll(io.LimitReader(file, 2<<20))
		if err != nil {
			detail(w, 500, "Internal Server Error")
			return
		}
		body = bytes.ReplaceAll(body, []byte("fetch('/api/accounts',"), []byte("fetch('/panel/api/pass-snapshot',"))
		body = bytes.ReplaceAll(body, []byte("Python / PID"), []byte("Go / PID"))
		body = bytes.ReplaceAll(body, []byte(", Telegram,"), []byte(","))
		body = bytes.ReplaceAll(body, []byte("person.kind === 'telegram' ? 'разрешено в Telegram' : "), nil)
		if info.Name() == "panel.js" {
			lines := [][]byte{}
			for _, line := range bytes.Split(body, []byte("\n")) {
				if !bytes.Contains(line, []byte("fact('Telegram',")) {
					lines = append(lines, line)
				}
			}
			body = bytes.Join(lines, []byte("\n"))
			body = bytes.ReplaceAll(body, []byte("bytes(s.files['sessions.sqlite3']) + ' + WAL ' + bytes(s.files['sessions.sqlite3-wal'])"), []byte("bytes(s.files.postgresql_bytes)"))
		}
		sum := md5.Sum(body)
		w.Header().Set("ETag", "\""+hex.EncodeToString(sum[:])+"\"")
		http.ServeContent(w, r, info.Name(), info.ModTime(), bytes.NewReader(body))
		return
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}
func (s *Server) staticRoutes() {
	for path, file := range map[string]string{"/": "index.html", "/today": "today.html", "/schedule": "schedule.html", "/panel": "panel.html"} {
		s.handle("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != path {
				detail(w, 404, "Not Found")
				return
			}
			s.asset(w, r, file, false)
		})
	}
	s.handle("GET /static/", func(w http.ResponseWriter, r *http.Request) {
		s.asset(w, r, strings.TrimPrefix(r.URL.Path, "/static/"), true)
	})
	for _, path := range []string{"/login", "/u/{id}"} {
		s.handle("GET "+path, func(w http.ResponseWriter, r *http.Request) { redirect(w, "/", 301) })
	}
}
