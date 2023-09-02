package packs

import (
	"fmt"
	"image"
	"image/color"
	"sync"

	"gioui.org/f32"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
	"gioui.org/x/component"
	"github.com/bedrock-tool/bedrocktool/ui/gui/pages"
	"github.com/bedrock-tool/bedrocktool/ui/messages"
	"github.com/bedrock-tool/bedrocktool/utils"
)

type (
	C = layout.Context
	D = layout.Dimensions
)

type Page struct {
	router *pages.Router

	finished  bool
	packsList widget.List
	l         sync.Mutex
	Packs     []*packEntry
}

type packEntry struct {
	IsFinished bool
	UUID       string

	HasIcon bool
	Icon    paint.ImageOp
	button  widget.Clickable

	Size   uint64
	Loaded uint64
	Name   string
	Path   string
	Err    error
}

func New(router *pages.Router) *Page {
	return &Page{
		router: router,
		packsList: widget.List{
			List: layout.List{
				Axis: layout.Vertical,
			},
		},
	}
}

func (p *Page) ID() string {
	return "packs"
}

var _ pages.Page = &Page{}

func (p *Page) Actions() []component.AppBarAction {
	return []component.AppBarAction{}
}

func (p *Page) Overflow() []component.OverflowAction {
	return []component.OverflowAction{}
}

func (p *Page) NavItem() component.NavItem {
	return component.NavItem{
		Name: "Pack Download",
		//Icon: icon.OtherIcon,
	}
}
func drawPackIcon(gtx C, hasImage bool, imageOp paint.ImageOp, bounds image.Point) D {
	return layout.Inset{
		Top:    unit.Dp(5),
		Bottom: unit.Dp(5),
		Right:  unit.Dp(5),
		Left:   unit.Dp(5),
	}.Layout(gtx, func(gtx C) D {
		if hasImage {
			imageOp.Add(gtx.Ops)
			s := imageOp.Size()
			p := f32.Pt(float32(s.X), float32(s.Y))
			p.X = 1 / (p.X / float32(bounds.X))
			p.Y = 1 / (p.Y / float32(bounds.Y))
			defer op.Affine(f32.Affine2D{}.Scale(f32.Pt(0, 0), p)).Push(gtx.Ops).Pop()
			paint.PaintOp{}.Add(gtx.Ops)
		}
		return D{Size: bounds}
	})
}

func MulAlpha(c color.NRGBA, alpha uint8) color.NRGBA {
	c.A = uint8(uint32(c.A) * uint32(alpha) / 0xFF)
	return c
}

func drawPackEntry(gtx C, th *material.Theme, pack *packEntry) D {
	var size = ""
	var colorSize = th.Palette.Fg
	if pack.IsFinished {
		size = utils.SizeofFmt(float32(pack.Size))
	} else {
		size = fmt.Sprintf("%s / %s  %.02f%%",
			utils.SizeofFmt(float32(pack.Loaded)),
			utils.SizeofFmt(float32(pack.Size)),
			float32(pack.Loaded)/float32(pack.Size)*100,
		)
		colorSize = color.NRGBA{0x00, 0xc9, 0xc9, 0xff}
	}

	return layout.UniformInset(5).Layout(gtx, func(gtx C) D {
		fn := func(gtx C) D {
			return layout.Stack{}.Layout(gtx,
				layout.Expanded(func(gtx layout.Context) layout.Dimensions {
					return component.Rect{
						Color: component.WithAlpha(th.Fg, 20),
						Size:  gtx.Constraints.Min,
						Radii: gtx.Dp(5),
					}.Layout(gtx)
				}),
				layout.Stacked(func(gtx C) D {
					return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
						layout.Rigid(func(gtx C) D {
							return drawPackIcon(gtx, pack.HasIcon, pack.Icon, image.Pt(50, 50))
						}),
						layout.Flexed(1, func(gtx C) D {
							return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
								layout.Rigid(material.Label(th, th.TextSize, pack.Name).Layout),
								layout.Rigid(material.LabelStyle{
									Text:           size,
									Color:          colorSize,
									SelectionColor: MulAlpha(th.Palette.ContrastBg, 0x60),
									TextSize:       th.TextSize,
									Shaper:         th.Shaper,
								}.Layout),
								layout.Rigid(func(gtx C) D {
									if pack.Err != nil {
										return material.LabelStyle{
											Color: color.NRGBA{0xbb, 0x00, 0x00, 0xff},
											Text:  pack.Err.Error(),
										}.Layout(gtx)
									}
									return D{}
								}),
							)
						}),
					)
				}),
			)
		}

		if pack.Path != "" {
			return material.ButtonLayoutStyle{
				Background:   MulAlpha(th.Palette.Bg, 0x60),
				Button:       &pack.button,
				CornerRadius: 3,
			}.Layout(gtx, fn)
		} else {
			return fn(gtx)
		}

	})
}

func (p *Page) layoutFinished(gtx C, th *material.Theme) D {
	for _, pack := range p.Packs {
		if pack.button.Clicked() {
			if pack.IsFinished {
				utils.ShowFile(pack.Path)
			}
		}
	}

	var title = "Downloading Packs"
	if p.finished {
		title = "Downloaded Packs"
	}

	return layout.Center.Layout(gtx, func(gtx C) D {
		return layout.Flex{
			Axis: layout.Vertical,
		}.Layout(gtx,
			layout.Rigid(material.Label(th, 20, title).Layout),
			layout.Flexed(1, func(gtx C) D {
				p.l.Lock()
				defer p.l.Unlock()

				return material.List(th, &p.packsList).Layout(gtx, len(p.Packs), func(gtx C, index int) D {
					pack := p.Packs[index]
					return drawPackEntry(gtx, th, pack)
				})
			}),
		)
	})
}

func (p *Page) Layout(gtx C, th *material.Theme) D {
	return layout.Inset{
		Top:    unit.Dp(25),
		Bottom: unit.Dp(25),
		Right:  unit.Dp(35),
		Left:   unit.Dp(35),
	}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return p.layoutFinished(gtx, th)
	})
}

func (p *Page) Handler(data interface{}) messages.MessageResponse {
	r := messages.MessageResponse{
		Ok:   false,
		Data: nil,
	}

	switch m := data.(type) {
	case messages.ConnectState:
		if m == messages.ConnectStateReceivingResources {
			p.router.RemovePopup("connect")
		}
	case messages.InitialPacksInfo:
		p.l.Lock()
		for _, dp := range m.Packs {
			p.Packs = append(p.Packs, &packEntry{
				IsFinished: false,
				UUID:       dp.UUID,
				Name:       dp.SubPackName + " v" + dp.Version,
				Size:       dp.Size,
			})
		}
		p.l.Unlock()
		p.router.Invalidate()

	case messages.PackDownloadProgress:
		p.l.Lock()
		for _, pe := range p.Packs {
			if pe.UUID == m.UUID {
				pe.Loaded += m.LoadedAdd
				if pe.Loaded == pe.Size {
					pe.IsFinished = true
				}
				break
			}
		}
		p.l.Unlock()
		p.router.Invalidate()

	case messages.FinishedPack:
		for _, pe := range p.Packs {
			if pe.UUID == m.Pack.UUID() {
				if m.Pack.Icon() != nil {
					pe.Icon = paint.NewImageOpFilter(m.Pack.Icon(), paint.FilterNearest)
					pe.HasIcon = true
				}
				pe.Loaded = pe.Size
				pe.IsFinished = true
				break
			}
		}

	case messages.FinishedDownloadingPacks:
		p.finished = true
		p.l.Lock()
		for _, pe := range p.Packs {
			dp, ok := m.Packs[pe.UUID]
			if !ok {
				continue
			}
			pe.Err = dp.Err
			pe.IsFinished = true
			pe.Path = dp.Path
		}
		p.l.Unlock()
		p.router.Invalidate()
		r.Ok = true
	}

	return r
}
