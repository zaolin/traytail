package main

// Icons are 32x32 ARGB32 pixmaps drawn at runtime; no asset files needed.
// ARGB32 is the SNI wire format (big-endian per pixel).

const iconSize = 32

func newPixmap(px [][][4]byte) []byte {
	buf := make([]byte, 0, iconSize*iconSize*4)
	for _, row := range px {
		for _, c := range row {
			// stored as {a, r, g, b}
			buf = append(buf, c[0], c[1], c[2], c[3])
		}
	}
	return buf
}

func blank() [][4]byte {
	row := make([][4]byte, iconSize)
	for i := range row {
		row[i] = [4]byte{255, 0, 0, 0}
	}
	return row
}

// iconConnected: white filled circle (opaque).
func iconConnected() []byte {
	px := make([]([][4]byte), iconSize)
	c := float64(iconSize-1) / 2
	r := float64(iconSize)/2 - 1
	for y := range px {
		row := make([][4]byte, iconSize)
		for x := range row {
			dx := float64(x) - c
			dy := float64(y) - c
			if dx*dx+dy*dy <= r*r {
				row[x] = [4]byte{255, 235, 235, 235}
			} else {
				row[x] = [4]byte{0, 0, 0, 0}
			}
		}
		px[y] = row
	}
	return newPixmap(px)
}

// iconExitNode: filled circle with a bold ring.
func iconExitNode() []byte {
	px := make([]([][4]byte), iconSize)
	c := float64(iconSize-1) / 2
	r := float64(iconSize)/2 - 1
	for y := range px {
		row := make([][4]byte, iconSize)
		for x := range row {
			dx := float64(x) - c
			dy := float64(y) - c
			d2 := dx*dx + dy*dy
			switch {
			case d2 <= r*r && d2 >= (r-3)*(r-3):
				row[x] = [4]byte{255, 90, 200, 100} // ring, green
			case d2 < (r-3)*(r-3) && d2 > (r-8)*(r-8):
				row[x] = [4]byte{255, 235, 235, 235} // dot
			default:
				row[x] = [4]byte{0, 0, 0, 0}
			}
		}
		px[y] = row
	}
	return newPixmap(px)
}

// iconOffline: hollow circle (outline only).
func iconOffline() []byte {
	px := make([]([][4]byte), iconSize)
	c := float64(iconSize-1) / 2
	r := float64(iconSize)/2 - 1
	for y := range px {
		row := make([][4]byte, iconSize)
		for x := range row {
			dx := float64(x) - c
			dy := float64(y) - c
			d2 := dx*dx + dy*dy
			if d2 <= r*r && d2 >= (r-2.5)*(r-2.5) {
				row[x] = [4]byte{160, 235, 235, 235} // semi-transparent outline
			} else {
				row[x] = [4]byte{0, 0, 0, 0}
			}
		}
		px[y] = row
	}
	return newPixmap(px)
}

// iconWarning: filled amber circle with a vertical bar (needs login).
func iconWarning() []byte {
	px := make([]([][4]byte), iconSize)
	c := float64(iconSize-1) / 2
	r := float64(iconSize)/2 - 1
	for y := range px {
		row := make([][4]byte, iconSize)
		for x := range row {
			dx := float64(x) - c
			dy := float64(y) - c
			if dx*dx+dy*dy <= r*r {
				if x >= 14 && x <= 17 && y >= 8 && y <= 22 {
					row[x] = [4]byte{255, 20, 20, 20}
				} else {
					row[x] = [4]byte{255, 240, 180, 40}
				}
			} else {
				row[x] = [4]byte{0, 0, 0, 0}
			}
		}
		px[y] = row
	}
	return newPixmap(px)
}
