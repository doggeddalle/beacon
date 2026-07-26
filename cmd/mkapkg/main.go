// Command mkapkg assembles an ASUSTOR App Central package (.apkg) from a staged
// project directory. An .apkg is a ZIP containing three members, in order:
//
//	apkg-version     the literal "2.0"
//	control.tar.gz   the CONTROL/ files (config.json, start-stop.sh, icon.png, …)
//	data.tar.gz      everything else (the app payload → /usr/local/AppCentral/<pkg>/)
//
// This is done with the Go standard library so it builds cross-platform (no
// Asustor apkg-tools required), and it sets the correct executable bits on the
// binary and scripts regardless of the host OS.
//
//	go run ./cmd/mkapkg -root <staged-dir> -out <file.apkg> -version <x.y.z>
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	root := flag.String("root", "", "staged project dir (contains CONTROL/ and payload)")
	out := flag.String("out", "", "output .apkg path")
	version := flag.String("version", "0.0.0", "package version (replaces APKG_VERSION in config.json)")
	flag.Parse()
	if *root == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: mkapkg -root <dir> -out <file.apkg> -version <x.y.z>")
		os.Exit(1)
	}
	if err := build(*root, *out, *version); err != nil {
		fmt.Fprintln(os.Stderr, "mkapkg:", err)
		os.Exit(1)
	}
	fmt.Println("built", *out)
}

type entry struct {
	name string // archive path, forward slashes, no leading "./"
	mode int64
	data []byte
}

func build(root, out, version string) error {
	control, data, err := collect(root, version)
	if err != nil {
		return err
	}
	if len(control) == 0 {
		return fmt.Errorf("no CONTROL files found under %s", root)
	}

	controlGz, err := tarGz(control)
	if err != nil {
		return err
	}
	dataGz, err := tarGz(data)
	if err != nil {
		return err
	}

	if dir := filepath.Dir(out); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	// Members are stored (not deflated): the tarballs are already gzipped, and
	// this matches the layout apkg-tools produces.
	add := func(name string, body []byte) error {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			return err
		}
		_, err = w.Write(body)
		return err
	}
	if err := add("apkg-version", []byte("2.0\n")); err != nil {
		return err
	}
	if err := add("control.tar.gz", controlGz); err != nil {
		return err
	}
	if err := add("data.tar.gz", dataGz); err != nil {
		return err
	}
	return zw.Close()
}

// collect walks root, splitting files into CONTROL (metadata/scripts) and data
// (payload). The version token in config.json is substituted here.
func collect(root, version string) (control, data []entry, err error) {
	root = filepath.Clean(root)
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		mode := int64(0o644)
		base := d.Name()
		if strings.HasSuffix(base, ".sh") || base == "beacon" {
			mode = 0o755
		}
		if strings.HasSuffix(base, ".sh") {
			// Guarantee LF line endings — a CRLF shebang breaks on the NAS.
			body = bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
			body = bytes.ReplaceAll(body, []byte("\r"), []byte("\n"))
		}

		if rel == "CONTROL" || strings.HasPrefix(rel, "CONTROL/") {
			name := strings.TrimPrefix(rel, "CONTROL/")
			if name == "config.json" {
				body = bytes.ReplaceAll(body, []byte("APKG_VERSION"), []byte(version))
			}
			control = append(control, entry{name: name, mode: mode, data: body})
		} else {
			data = append(data, entry{name: rel, mode: mode, data: body})
		}
		return nil
	})
	return control, data, err
}

func tarGz(entries []entry) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	now := time.Now()
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     "./" + e.name,
			Mode:     e.mode,
			Size:     int64(len(e.data)),
			ModTime:  now,
			Typeflag: tar.TypeReg,
			Uname:    "root",
			Gname:    "root",
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(e.data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
