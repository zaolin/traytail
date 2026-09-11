package main

import (
	"testing"
)

func checkPixmap(t *testing.T, px []byte, name string) {
	t.Helper()
	if len(px) != iconSize*iconSize*4 {
		t.Fatalf("%s: %d bytes, want %d", name, len(px), iconSize*iconSize*4)
	}
}

// pixelAt decodes the ARGB32 pixel at (x, y) into {a,r,g,b}.
func pixelAt(t *testing.T, px []byte, x, y int) [4]byte {
	t.Helper()
	if x < 0 || x >= iconSize || y < 0 || y >= iconSize {
		t.Fatalf("pixel (%d,%d) out of bounds", x, y)
	}
	off := (y*iconSize + x) * 4
	return [4]byte{px[off], px[off+1], px[off+2], px[off+3]}
}

func TestIconConnected(t *testing.T) {
	px := iconConnected()
	checkPixmap(t, px, "connected")
	center := pixelAt(t, px, 16, 16)
	if center != [4]byte{255, 235, 235, 235} {
		t.Errorf("center = %v, want opaque white", center)
	}
	corner := pixelAt(t, px, 0, 0)
	if corner != [4]byte{0, 0, 0, 0} {
		t.Errorf("corner = %v, want transparent", corner)
	}
	// all rows should be the same length: buffer is contiguous
	if px[0] != 0 { // corner row starts transparent
		t.Error("first pixel should be transparent")
	}
}

func TestIconExitNode(t *testing.T) {
	px := iconExitNode()
	checkPixmap(t, px, "exit node")
	// ring pixel: on radius edge near the top of the circle (center 15.5, r=15)
	ring := pixelAt(t, px, 16, 1)
	if ring != [4]byte{255, 90, 200, 100} {
		t.Errorf("ring = %v, want green", ring)
	}
	// dot pixel: x=8 (offset 7.5 from center) lands in the white annulus
	// (dot band is where (r-8)² < d2 < (r-3)², i.e. x offsets 8..11)
	dot := pixelAt(t, px, 8, 16)
	if dot != [4]byte{255, 235, 235, 235} {
		t.Errorf("dot = %v, want white", dot)
	}
	// gap between ring and dot (annulus hole): x=9 is inside the hole
	hole := pixelAt(t, px, 9, 16)
	if hole != [4]byte{0, 0, 0, 0} {
		t.Errorf("hole = %v, want transparent", hole)
	}
}

func TestIconOffline(t *testing.T) {
	px := iconOffline()
	checkPixmap(t, px, "offline")
	center := pixelAt(t, px, 16, 16)
	if center != [4]byte{0, 0, 0, 0} {
		t.Errorf("center = %v, want transparent (hollow)", center)
	}
	edge := pixelAt(t, px, 16, 1)
	if edge != [4]byte{160, 235, 235, 235} {
		t.Errorf("outline = %v, want semi-transparent", edge)
	}
}

func TestIconWarning(t *testing.T) {
	px := iconWarning()
	checkPixmap(t, px, "warning")
	bar := pixelAt(t, px, 15, 15)
	if bar != [4]byte{255, 20, 20, 20} {
		t.Errorf("bar = %v, want dark", bar)
	}
	body := pixelAt(t, px, 6, 16)
	if body != [4]byte{255, 240, 180, 40} {
		t.Errorf("body = %v, want amber", body)
	}
	corner := pixelAt(t, px, 31, 31)
	if corner != [4]byte{0, 0, 0, 0} {
		t.Errorf("corner = %v, want transparent", corner)
	}
}

func TestBlank(t *testing.T) {
	rows := blank()
	if len(rows) != iconSize {
		t.Fatalf("blank rows = %d", len(rows))
	}
	for i, px := range rows {
		if px != [4]byte{255, 0, 0, 0} {
			t.Fatalf("row %d pixel = %v", i, px)
		}
	}
}

func TestNewPixmapSerialization(t *testing.T) {
	px := [][][4]byte{{{1, 2, 3, 4}, {5, 6, 7, 8}}}
	got := newPixmap(px)
	want := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	for i, b := range want {
		if got[i] != b {
			t.Fatalf("newPixmap[%d] = %d, want %d", i, got[i], b)
		}
	}
}