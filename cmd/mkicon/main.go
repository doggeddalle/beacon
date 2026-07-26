// Command mkicon generates Beacon's App Central icon (a cyan ◈ beacon mark on a
// dark rounded square) as a PNG. Run during packaging so no binary asset needs
// to be committed.
//
//	go run ./cmd/mkicon path/to/icon.png [size]
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"strconv"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mkicon <out.png> [size]")
		os.Exit(1)
	}
	out := os.Args[1]
	size := 256
	if len(os.Args) > 2 {
		if n, err := strconv.Atoi(os.Args[2]); err == nil && n > 0 {
			size = n
		}
	}
	if err := write(out, size); err != nil {
		fmt.Fprintln(os.Stderr, "mkicon:", err)
		os.Exit(1)
	}
}

func write(out string, size int) error {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	var (
		bg    = color.RGBA{0x0f, 0x1b, 0x2e, 0xff} // dark navy
		cyan  = color.RGBA{0x38, 0xbd, 0xf8, 0xff} // accent
		clear = color.RGBA{0, 0, 0, 0}
	)
	radius := size / 5
	cx, cy := size/2, size/2
	outer := size * 32 / 100 // outer diamond "radius" (Manhattan)
	hole := size * 18 / 100
	dot := size * 6 / 100

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if !roundedRectContains(x, y, size, size, radius) {
				img.Set(x, y, clear)
				continue
			}
			img.Set(x, y, bg)
			d := abs(x-cx) + abs(y-cy) // diamond distance
			switch {
			case d < dot:
				img.Set(x, y, cyan) // centre dot
			case d < hole:
				// ring hole (keep bg)
			case d < outer:
				img.Set(x, y, cyan) // outer diamond
			}
		}
	}

	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

// roundedRectContains reports whether (x,y) is inside a w×h rounded rectangle
// with the given corner radius.
func roundedRectContains(x, y, w, h, r int) bool {
	rx, ry := x, y
	if x < r {
		rx = r
	} else if x >= w-r {
		rx = w - r - 1
	}
	if y < r {
		ry = r
	} else if y >= h-r {
		ry = h - r - 1
	}
	dx, dy := x-rx, y-ry
	return dx*dx+dy*dy <= r*r
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
