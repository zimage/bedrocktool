package utils

import (
	"image"
	"image/color"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

func isBlockLightblocking(b world.Block) bool {
	d, isDiffuser := b.(block.LightDiffuser)
	noDiffuse := isDiffuser && d.LightDiffusionLevel() == 0
	return !noDiffuse
}

func blockColorAt(c *chunk.Chunk, x uint8, y int16, z uint8) (blockColor color.RGBA) {
	if y <= int16(c.Range().Min()) {
		return color.RGBA{0, 0, 0, 0}
	}
	rid := c.Block(x, y, z, 0)

	blockColor = color.RGBA{255, 0, 255, 255}
	b, found := world.BlockByRuntimeID(rid)
	if !found {
		return blockColor
	}

	if _, isWater := b.(block.Water); isWater {
		waterColor := block.Water{}.Color()

		// get the first non water block at the position
		heightBlock := c.HeightMap().At(x, z)
		depth := y - heightBlock
		if depth > 0 {
			blockColor = blockColorAt(c, x, heightBlock, z)
		}

		// blend that blocks color with water depending on depth
		waterColor.A = uint8(Clamp(int(150+depth*7), 255))
		blockColor = BlendColors(blockColor, waterColor)
		blockColor.R -= uint8(depth * 2)
		blockColor.G -= uint8(depth * 2)
		blockColor.B -= uint8(depth * 2)
		return blockColor
	} else {
		col := b.Color()
		if col.A != 255 {
			col = BlendColors(blockColorAt(c, x, y-1, z), col)
		}

		/*
			a := color.RGBA{255, 0, 255, 255}
			if col == a {
				name, nbt := b.EncodeBlock()
				fmt.Printf("unknown color %d  %s %s %s\n", rid, reflect.TypeOf(b), name, nbt)
			}
		*/

		return col
	}
}

func chunkGetColorAt(c *chunk.Chunk, x uint8, y int16, z uint8) color.RGBA {
	haveUp := false
	cube.Pos{int(x), int(y), int(z)}.
		Side(cube.FaceUp).
		Neighbours(func(neighbour cube.Pos) {
			if neighbour.X() < 0 || neighbour.X() >= 16 || neighbour.Z() < 0 || neighbour.Z() >= 16 || neighbour.Y() > c.Range().Max() || haveUp {
				return
			}
			blockRid := c.Block(uint8(neighbour[0]), int16(neighbour[1]), uint8(neighbour[2]), 0)
			if blockRid > 0 {
				b, found := world.BlockByRuntimeID(blockRid)
				if found {
					if isBlockLightblocking(b) {
						haveUp = true
					}
				}
			}
		}, cube.Range{int(y + 1), int(y + 1)})

	blockColor := blockColorAt(c, x, y, z)
	if haveUp && (x+z)%2 == 0 {
		if blockColor.R > 10 {
			blockColor.R -= 10
		}
		if blockColor.G > 10 {
			blockColor.G -= 10
		}
		if blockColor.B > 10 {
			blockColor.B -= 10
		}
	}
	return blockColor
}

func Chunk2Img(c *chunk.Chunk) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	hm := c.HeightMapWithWater()

	for x := uint8(0); x < 16; x++ {
		for z := uint8(0); z < 16; z++ {
			img.SetRGBA(
				int(x), int(z),
				chunkGetColorAt(c, x, hm.At(x, z), z),
			)
		}
	}
	return img
}
