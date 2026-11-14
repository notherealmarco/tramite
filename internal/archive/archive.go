// Package archive stores raw feed versions on disk for later analysis:
// raw/<source>/<feed>/<YYYY-MM-DD>/<HHMMSS>_<feed timestamp>.pb.gz
package archive

import (
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Archive struct{ Dir string }

// Save writes one feed version. ts is the feed's own timestamp (0 if none).
func (a Archive) Save(source, feed string, fetched, ts time.Time, body []byte) error {
	day := fetched.Format("2006-01-02")
	dir := filepath.Join(a.Dir, "raw", source, feed, day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var unix int64
	if !ts.IsZero() {
		unix = ts.Unix()
	}
	name := filepath.Join(dir, fmt.Sprintf("%s_%d.pb.gz", fetched.Format("150405"), unix))
	tmp := name + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(f)
	if _, err := zw.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, name)
}
