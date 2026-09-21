package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/thesahibnanda-max/relay/internal/ids"
	"github.com/thesahibnanda-max/relay/internal/proto"
)

var segmentRe = regexp.MustCompile(`^raw-(\d{4})\.jsonl$`)

// compressFile writes path.zst atomically and removes the original. It
// returns the bytes saved. The raw logs are terminal output: highly repetitive,
// so zstd typically shrinks them several-fold.
func compressFile(path string) (saved int64, err error) {
	in, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return 0, err
	}
	tmp := path + ".zst.tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	enc, err := zstd.NewWriter(out)
	if err != nil {
		out.Close()
		os.Remove(tmp)
		return 0, err
	}
	_, err = io.Copy(enc, in)
	if cerr := enc.Close(); err == nil {
		err = cerr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return 0, err
	}
	zst, _ := os.Stat(tmp)
	if err := os.Rename(tmp, path+".zst"); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if err := os.Remove(path); err != nil {
		return 0, err
	}
	return st.Size() - zst.Size(), nil
}

// bgCompress compresses a just-closed segment without delaying the ack path.
type bgCompress struct{ wg sync.WaitGroup }

func (b *bgCompress) run(path string) {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		_, _ = compressFile(path) // best effort: an uncompressed segment is still valid
	}()
}

func dirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if i, err := d.Info(); err == nil {
				n += i.Size()
			}
		}
		return nil
	})
	return n
}

// GC prunes idle sessions and compresses closed raw segments.
func (s *Server) GC(ctx context.Context, req proto.GCRequest) (proto.GCReport, error) {
	rep := proto.GCReport{DryRun: req.DryRun}
	now := time.Now()

	if req.OlderThanS > 0 {
		cutoff := now.Add(-time.Duration(req.OlderThanS) * time.Second)
		idle, err := s.st.IdleSessions(ctx, cutoff)
		if err != nil {
			return rep, err
		}
		for _, sess := range idle {
			c, err := s.st.CountSession(ctx, sess.ID)
			if err != nil {
				return rep, err
			}
			rawDir := filepath.Join(s.opt.Paths.RawDir(), sess.ID)
			rep.Sessions++
			rep.Agents += c.Agents
			rep.Messages += c.Messages
			rep.Events += c.Events
			rep.RawBytesFreed += dirSize(rawDir)
			if req.DryRun {
				continue
			}
			if err := s.st.DeleteSession(ctx, sess.ID); err != nil {
				return rep, err
			}
			if ids.Valid(sess.ID) { // never build a path from anything but a ULID
				_ = os.RemoveAll(rawDir)
			}
		}
		// The agent-side local logs (sessions/*.jsonl) are per process, not per session: go by age.
		if entries, err := os.ReadDir(s.opt.Paths.SessionsDir()); err == nil {
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
					continue
				}
				if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
					rep.LocalLogsRemoved++
					rep.RawBytesFreed += info.Size()
					if !req.DryRun {
						_ = os.Remove(filepath.Join(s.opt.Paths.SessionsDir(), e.Name()))
					}
				}
			}
		}
	}

	if req.Compress {
		sessions, _ := os.ReadDir(s.opt.Paths.RawDir())
		for _, sd := range sessions {
			agents, _ := os.ReadDir(filepath.Join(s.opt.Paths.RawDir(), sd.Name()))
			for _, ad := range agents {
				dir := filepath.Join(s.opt.Paths.RawDir(), sd.Name(), ad.Name())
				keepOpen := 0 // an agent that may still write keeps its newest segment uncompressed
				if a, err := s.st.GetAgent(ctx, ad.Name()); err == nil && a.Status != "exited" {
					keepOpen = 1
				}
				var segs []string
				files, _ := os.ReadDir(dir)
				for _, f := range files {
					if segmentRe.MatchString(f.Name()) {
						segs = append(segs, f.Name())
					}
				}
				for i, name := range segs { // ReadDir is sorted, and the names sort by number
					if i >= len(segs)-keepOpen {
						break
					}
					path := filepath.Join(dir, name)
					if _, err := os.Stat(path + ".zst"); err == nil {
						continue
					}
					if req.DryRun {
						if st, err := os.Stat(path); err == nil {
							rep.SegmentsCompressed++
							rep.BytesSaved += st.Size() * 3 / 4 // an estimate
						}
						continue
					}
					saved, err := compressFile(path)
					if err != nil {
						s.log.Warn("compress segment", "path", path, "err", err)
						continue
					}
					rep.SegmentsCompressed++
					rep.BytesSaved += saved
				}
			}
		}
	}
	return rep, nil
}

func (s *Server) handleGC(w http.ResponseWriter, r *http.Request) {
	var req proto.GCRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil || req.OlderThanS < 0 {
		writeJSON(w, 400, proto.APIError{Error: "bad request"})
		return
	}
	rep, err := s.GC(r.Context(), req)
	if err != nil {
		s.log.Error("gc", "err", err)
		writeJSON(w, 500, proto.APIError{Error: fmt.Sprintf("gc failed: %v", err)})
		return
	}
	s.log.Info("gc", "sessions", rep.Sessions, "compressed", rep.SegmentsCompressed, "dry_run", rep.DryRun)
	writeJSON(w, 200, rep)
}
