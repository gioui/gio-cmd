// SPDX-License-Identifier: Unlicense OR MIT

// Command svg2gio converts SVG files to Gio functions. Only a limited subset of
// SVG files are supported.
package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"gioui.org/f32"
)

var (
	pkg    = flag.String("pkg", "", "Go package")
	output = flag.String("o", "svg.go", "Output Go file")
)

func main() {
	flag.Parse()
	if *pkg == "" {
		fmt.Fprintf(os.Stderr, "specify a package name (-pkg)\n")
		os.Exit(1)
	}
	args := flag.Args()
	if err := convertAll(args); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
}

type Points []float32

func (p *Points) UnmarshalText(text []byte) error {
	for {
		text = bytes.TrimLeft(text, "\t\n")
		if len(text) == 0 {
			break
		}
		var num []byte
		end := bytes.IndexAny(text, " ,")
		if end != -1 {
			num = text[:end]
			text = text[end+1:]
		} else {
			num = text
			text = nil
		}
		f, err := strconv.ParseFloat(string(num), 32)
		if err != nil {
			return err
		}
		*p = append(*p, float32(f))
	}
	return nil
}

type Transform f32.Affine2D

func (t *Transform) UnmarshalText(text []byte) error {
	switch {
	case bytes.HasPrefix(text, []byte("matrix(")) && bytes.HasSuffix(text, []byte(")")):
		trans := text[7 : len(text)-1]
		var p Points
		if err := p.UnmarshalText(trans); err != nil {
			return err
		}
		if len(p) != 6 {
			return fmt.Errorf("malformed transform matrix: %q", text)
		}
		*t = Transform(f32.NewAffine2D(p[0], p[2], p[4], p[1], p[3], p[5]))
		return nil
	default:
		return fmt.Errorf("unsupported transform: %q", text)
	}
}

type Fill struct {
	Transform      Transform `xml:"transform,attr"`
	Fill           Color     `xml:"fill,attr"`
	Stroke         Color     `xml:"stroke,attr"`
	StrokeLinejoin string    `xml:"stroke-linejoin,attr"`
	StrokeLinecap  string    `xml:"stroke-linecap,attr"`
	StrokeWidth    float32   `xml:"stroke-width,attr"`
}

type Color struct {
	Set   bool
	Value int
}

func (c *Color) UnmarshalText(text []byte) error {
	if string(text) == "none" {
		*c = Color{}
		return nil
	}
	if !bytes.HasPrefix(text, []byte("#")) {
		return fmt.Errorf("invalid color: %q", text)
	}
	text = text[1:]
	i, err := strconv.ParseInt(string(text), 16, 32)
	// Implied alpha.
	if len(text) == 6 {
		i |= 0xff000000
	}
	*c = Color{
		Set:   true,
		Value: int(i),
	}
	return err
}

func convertAll(files []string) error {
	w := new(bytes.Buffer)
	fmt.Fprintf(w, "// Code generated by gioui.org/cmd/svg2gio; DO NOT EDIT.\n\n")
	fmt.Fprintf(w, "package %s\n\n", *pkg)
	fmt.Fprintf(w, "import \"image/color\"\n")
	fmt.Fprintf(w, "import \"math\"\n")
	fmt.Fprintf(w, "import \"gioui.org/op\"\n")
	fmt.Fprintf(w, "import \"gioui.org/op/clip\"\n")
	fmt.Fprintf(w, "import \"gioui.org/op/paint\"\n")
	fmt.Fprintf(w, "import \"gioui.org/f32\"\n\n")
	fmt.Fprintf(w, "var ops op.Ops\n\n")
	fmt.Fprintf(w, funcs)
	for _, filename := range files {
		if err := convert(w, filename); err != nil {
			return err
		}
	}
	src, err := format.Source(w.Bytes())
	if err != nil {
		return err
	}
	return os.WriteFile(*output, src, 0o660)
}

func convert(w io.Writer, filename string) error {
	base := filepath.Base(filename)
	ext := filepath.Ext(base)
	name := "Image_" + base[:len(base)-len(ext)]

	fmt.Fprintf(w, "var %s struct {\n", name)
	fmt.Fprintf(w, "ViewBox struct { Min, Max f32.Point }\n")
	fmt.Fprintf(w, "Call op.CallOp\n\n")
	fmt.Fprintf(w, "}\n")
	fmt.Fprintf(w, "func init() {\n")
	defer fmt.Fprintf(w, "}\n")
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	d := xml.NewDecoder(f)
	if err := parse(w, d, name); err != nil {
		line, col := d.InputPos()
		return fmt.Errorf("%s:%d:%d: %w", filename, line, col, err)
	}
	return nil
}

func parse(w io.Writer, d *xml.Decoder, name string) error {
	for {
		tok, err := d.Token()
		if err != nil {
			if err == io.EOF {
				return errors.New("unexpected end of file")
			}
			return err
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			if n := tok.Name.Local; n != "svg" {
				return fmt.Errorf("invalid SVG root: <%s>", n)
			}
			if n := tok.Name.Space; n != "http://www.w3.org/2000/svg" {
				return fmt.Errorf("unsupported SVG namespace: %s", n)
			}
			fmt.Fprintf(w, "m := op.Record(&ops)\n")
			defer fmt.Fprintf(w, "%s.Call = m.Stop()\n", name)
			for _, a := range tok.Attr {
				if a.Name.Local == "viewBox" {
					var p Points
					if err := p.UnmarshalText([]byte(a.Value)); err != nil {
						return fmt.Errorf("invalid viewBox attribute: %s", a.Value)
					}
					if len(p) != 4 {
						return fmt.Errorf("invalid viewBox attribute: %s", a.Value)
					}
					fmt.Fprintf(w, "%s.ViewBox.Min = %s\n", name, point(f32.Pt(p[0], p[1])))
					fmt.Fprintf(w, "%s.ViewBox.Max = %s\n", name, point(f32.Pt(p[2], p[3])))
				}
			}
			return parseSVG(w, d)
		}
	}
}

func point(p f32.Point) string {
	return fmt.Sprintf("f32.Pt(%g, %g)", p.X, p.Y)
}

type Poly struct {
	XMLName xml.Name
	Points  Points `xml:"points,attr"`
	Fill
}

func (p *Poly) Path(w io.Writer) error {
	if len(p.Points) <= 1 {
		return nil
	}
	pen := f32.Pt(p.Points[0], p.Points[1])
	fmt.Fprintf(w, "p.MoveTo(%s)\n", point(pen))
	last := pen
	for i := 2; i < len(p.Points); i += 2 {
		last = f32.Pt(p.Points[i], p.Points[i+1])
		fmt.Fprintf(w, "p.LineTo(%s)\n", point(last))
	}
	if p.XMLName.Local == "polygon" && last != pen {
		fmt.Fprintf(w, "p.LineTo(%s)\n", point(pen))
	}
	return nil
}

type Path struct {
	D string `xml:"d,attr"`
	Fill
}

func (p *Path) Path(w io.Writer) error {
	return printPathCommands(w, p.D)
}

type Line struct {
	X1 float32 `xml:"x1,attr"`
	Y1 float32 `xml:"y1,attr"`
	X2 float32 `xml:"x2,attr"`
	Y2 float32 `xml:"y2,attr"`
	Fill
}

func (l *Line) Path(w io.Writer) error {
	fmt.Fprintf(w, "p.MoveTo(%s)\n", point(f32.Pt(l.X1, l.Y1)))
	fmt.Fprintf(w, "p.LineTo(%s)\n", point(f32.Pt(l.X2, l.Y2)))
	return nil
}

type Ellipse struct {
	Cx float32 `xml:"cx,attr"`
	Cy float32 `xml:"cy,attr"`
	Rx float32 `xml:"rx,attr"`
	Ry float32 `xml:"ry,attr"`
	Fill
}

func (e *Ellipse) Path(w io.Writer) error {
	c := f32.Pt(e.Cx, e.Cy)
	r := f32.Pt(e.Rx, e.Ry)
	fmt.Fprintf(w, "ellipse(&p, %s, %s)\n", point(c), point(r))
	return nil
}

type Rect struct {
	X      float32 `xml:"x,attr"`
	Y      float32 `xml:"y,attr"`
	Width  float32 `xml:"width,attr"`
	Height float32 `xml:"height,attr"`
	Fill
}

func (r *Rect) Path(w io.Writer) error {
	o := f32.Pt(r.X, r.Y)
	sz := f32.Pt(r.Width, r.Height)
	fmt.Fprintf(w, "rect(&p, %s, %s)\n", point(o), point(sz))
	return nil
}

type Circle struct {
	Cx float32 `xml:"cx,attr"`
	Cy float32 `xml:"cy,attr"`
	R  float32 `xml:"r,attr"`
	Fill
}

func (c *Circle) Path(w io.Writer) error {
	center := f32.Pt(c.Cx, c.Cy)
	r := f32.Pt(c.R, c.R)
	fmt.Fprintf(w, "ellipse(&p, %s, %s)\n", point(center), point(r))
	return nil
}

func parseSVG(w io.Writer, d *xml.Decoder) error {
	for {
		tok, err := d.Token()
		if err != nil {
			if err == io.EOF {
				return errors.New("unexpected end of <svg> element")
			}
			return err
		}
		var start xml.StartElement
		switch tok := tok.(type) {
		case xml.EndElement:
			return nil
		case xml.StartElement:
			start = tok
		default:
			continue
		}
		var elem interface {
			Path(w io.Writer) error
		}
		var fill *Fill
		switch n := start.Name.Local; n {
		case "g":
			// Flatten groups.
			if err := parseSVG(w, d); err != nil {
				return err
			}
			continue
		case "title":
			d.Skip()
			continue
		case "polygon", "polyline":
			p := new(Poly)
			elem = p
			fill = &p.Fill
		case "path":
			p := new(Path)
			elem = p
			fill = &p.Fill
		case "line":
			l := new(Line)
			elem = l
			fill = &l.Fill
		case "ellipse":
			e := new(Ellipse)
			elem = e
			fill = &e.Fill
		case "rect":
			r := new(Rect)
			elem = r
			fill = &r.Fill
		case "circle":
			c := new(Circle)
			elem = c
			fill = &c.Fill
		default:
			return fmt.Errorf("unsupported tag: <%s>", n)
		}
		if err := d.DecodeElement(elem, &start); err != nil {
			return err
		}
		if !fill.Fill.Set && !fill.Stroke.Set {
			continue
		}
		fmt.Fprintf(w, "{\n")
		trans := f32.Affine2D(fill.Transform)
		if trans != (f32.Affine2D{}) {
			sx, hx, ox, sy, hy, oy := trans.Elems()
			fmt.Fprintf(w, "t := op.Affine(f32.NewAffine2D(%g, %g, %g, %g, %g, %g)).Push(&ops)\n", sx, hx, ox, sy, hy, oy)
		}
		fmt.Fprintf(w, "var p clip.Path\n")
		fmt.Fprintf(w, "p.Begin(&ops)\n")
		if err := elem.Path(w); err != nil {
			return err
		}
		fmt.Fprintf(w, "spec := p.End()\n")
		if fill.Fill.Set {
			fmt.Fprintf(w, "paint.FillShape(&ops, argb(%#.8x), clip.Outline{Path: spec}.Op())\n", fill.Fill.Value)
		}
		if fill.Stroke.Set {
			fmt.Fprintf(w, "paint.FillShape(&ops, argb(%#.8x), clip.Stroke{Width: %g, Path: spec}.Op())\n", fill.Stroke.Value, fill.StrokeWidth)
		}
		if trans != (f32.Affine2D{}) {
			fmt.Fprintf(w, "t.Pop()\n")
		}
		fmt.Fprintf(w, "}\n")
	}
}

func printPathCommands(w io.Writer, cmds string) error {
	moveTo := func(p f32.Point) {
		fmt.Fprintf(w, "p.MoveTo(%s)\n", point(p))
	}
	lineTo := func(p f32.Point) {
		fmt.Fprintf(w, "p.LineTo(%s)\n", point(p))
	}
	cubeTo := func(p0, p1, p2 f32.Point) {
		fmt.Fprintf(w, "p.CubeTo(%s, %s, %s)\n", point(p0), point(p1), point(p2))
	}
	cmds = strings.TrimSpace(cmds)
	var pen f32.Point
	initPoint := pen
	ctrl2 := pen
	for {
		cmds = strings.TrimLeft(cmds, " ,\t\n")
		if len(cmds) == 0 {
			break
		}
		orig := cmds
		op := rune(cmds[0])
		cmds = cmds[1:]
		switch op {
		case 'M', 'm', 'V', 'v', 'L', 'l', 'H', 'h', 'C', 'c', 'S', 's':
		case 'Z', 'z':
			if pen != initPoint {
				lineTo(initPoint)
				pen = initPoint
			}
			ctrl2 = initPoint
			continue
		default:
			return fmt.Errorf("unknown <path> command %s in %q", string(op), orig)
		}
		var coords []float64
		for {
			cmds = strings.TrimLeft(cmds, " ,\t\n")
			if len(cmds) == 0 {
				break
			}
			n, x, ok := parseFloat(cmds)
			if !ok {
				break
			}
			cmds = cmds[n:]
			coords = append(coords, x)
		}
		rel := unicode.IsLower(op)
		newPen := pen
		switch unicode.ToLower(op) {
		case 'h':
			for _, x := range coords {
				p := f32.Pt(float32(x), pen.Y)
				if rel {
					p.X += pen.X
				}
				lineTo(p)
				newPen = p
			}
			pen = newPen
			ctrl2 = newPen
			continue
		case 'v':
			for _, y := range coords {
				p := f32.Pt(pen.X, float32(y))
				if rel {
					p.Y += pen.Y
				}
				lineTo(p)
				newPen = p
			}
			pen = newPen
			ctrl2 = newPen
			continue
		}
		if len(coords)%2 != 0 {
			return fmt.Errorf("odd number of coordinates in <path> data: %q", orig)
		}
		var off f32.Point
		if rel {
			// Relative command.
			off = pen
		} else {
			off = f32.Pt(0, 0)
		}
		var points []f32.Point
		for i := 0; i < len(coords); i += 2 {
			p := f32.Pt(float32(coords[i]), float32(coords[i+1]))
			p = p.Add(off)
			points = append(points, p)
		}
		newCtrl2 := ctrl2
		switch op := unicode.ToLower(op); op {
		case 'm', 'l':
			sop := moveTo
			if op == 'l' {
				sop = lineTo
			}
			for _, p := range points {
				sop(p)
				newPen = p
			}
			if op == 'm' {
				initPoint = newPen
			}
		case 'c':
			for i := 0; i < len(points); i += 3 {
				p1, p2, p3 := points[i], points[i+1], points[i+2]
				cubeTo(p1, p2, p3)
				newPen = p3
				newCtrl2 = p2
			}
		case 's':
			for i := 0; i < len(points); i += 2 {
				p2, p3 := points[i], points[i+1]
				// Compute p1 by reflecting p2 on to the line that contains pen and p2.
				p1 := pen.Mul(2).Sub(ctrl2)
				cubeTo(p1, p2, p3)
				newPen = p3
				newCtrl2 = p2
			}
		}
		pen = newPen
		ctrl2 = newCtrl2
	}
	return nil
}

func parseFloat(s string) (int, float64, bool) {
	n := 0
	if len(s) > 0 && s[0] == '-' {
		n++
	}
	for ; n < len(s); n++ {
		if !(unicode.IsDigit(rune(s[n])) || s[n] == '.') {
			break
		}
	}
	f, err := strconv.ParseFloat(s[:n], 64)
	return n, f, err == nil
}

const funcs = `
func argb(c uint32) color.NRGBA {
	return color.NRGBA{A: uint8(c >> 24), R: uint8(c >> 16), G: uint8(c >> 8), B: uint8(c)}
}

func rect(p *clip.Path, origin, size f32.Point) {
	p.MoveTo(origin)
	p.LineTo(origin.Add(f32.Pt(size.X, 0)))
	p.LineTo(origin.Add(size))
	p.LineTo(origin.Add(f32.Pt(0, size.Y)))
	p.Close()
}

func ellipse(p *clip.Path, center, radius f32.Point) {
	r := radius.X
	// We'll model the ellipse as a circle scaled in the Y
	// direction.
	scale := radius.Y / r

	// https://pomax.github.io/bezierinfo/#circles_cubic.
	const q = 4 * (math.Sqrt2 - 1) / 3

	curve := r * q
	top := f32.Point{X: center.X, Y: center.Y - r*scale}

	p.MoveTo(top)
	p.CubeTo(
		f32.Point{X: center.X + curve, Y: center.Y - r*scale},
		f32.Point{X: center.X + r, Y: center.Y - curve*scale},
		f32.Point{X: center.X + r, Y: center.Y},
	)
	p.CubeTo(
		f32.Point{X: center.X + r, Y: center.Y + curve*scale},
		f32.Point{X: center.X + curve, Y: center.Y + r*scale},
		f32.Point{X: center.X, Y: center.Y + r*scale},
	)
	p.CubeTo(
		f32.Point{X: center.X - curve, Y: center.Y + r*scale},
		f32.Point{X: center.X - r, Y: center.Y + curve*scale},
		f32.Point{X: center.X - r, Y: center.Y},
	)
	p.CubeTo(
		f32.Point{X: center.X - r, Y: center.Y - curve*scale},
		f32.Point{X: center.X - curve, Y: center.Y - r*scale},
		top,
	)
}
`
