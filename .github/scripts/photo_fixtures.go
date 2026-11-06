// Generate real, decodable JPEG and PNG files for the manual VK curl probe.
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) != 2 {
		panic("usage: photo_fixtures <output directory>")
	}
	if err := os.MkdirAll(os.Args[1], 0700); err != nil {
		panic(err)
	}
	for _, width := range []int{320, 1280} {
		height := width * 3 / 4
		img := image.NewRGBA(image.Rect(0, 0, width, height))
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				img.SetRGBA(x, y, color.RGBA{uint8(x * 255 / width), uint8(y * 255 / height), uint8((x/16 + y/16) % 2 * 255), 255})
			}
		}
		for _, ext := range []string{"jpg", "png"} {
			name := filepath.Join(os.Args[1], fmt.Sprintf("photo-%d.%s", width, ext))
			file, err := os.Create(name)
			if err != nil {
				panic(err)
			}
			if ext == "jpg" {
				err = jpeg.Encode(file, img, &jpeg.Options{Quality: 90})
			} else {
				err = png.Encode(file, img)
			}
			closeErr := file.Close()
			if err != nil {
				panic(err)
			}
			if closeErr != nil {
				panic(closeErr)
			}
		}
	}
}
