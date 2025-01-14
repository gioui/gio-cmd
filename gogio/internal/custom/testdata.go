// SPDX-License-Identifier: Unlicense OR MIT

//go:build linux
// +build linux

// This program demonstrates the use of a custom OpenGL ES context with
// app.Window.
package main

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"log"
	"os"
	"runtime"
	"strings"
	"unsafe"

	"gioui.org/app"
	"gioui.org/gpu"
	"gioui.org/io/event"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
)

/*
#cgo linux pkg-config: egl wayland-egl
#cgo freebsd openbsd CFLAGS: -I/usr/local/include
#cgo openbsd CFLAGS: -I/usr/X11R6/include
#cgo freebsd LDFLAGS: -L/usr/local/lib
#cgo openbsd LDFLAGS: -L/usr/X11R6/lib
#cgo freebsd openbsd LDFLAGS: -lwayland-egl
#cgo CFLAGS: -DEGL_NO_X11
#cgo LDFLAGS: -lEGL -lGLESv2

#include <EGL/egl.h>
#include <wayland-client.h>
#include <wayland-egl.h>
#include <GLES3/gl3.h>
#define EGL_EGLEXT_PROTOTYPES
#include <EGL/eglext.h>

*/
import "C"

func getDisplay(ve app.ViewEvent) C.EGLDisplay {
	switch ve := ve.(type) {
	case app.X11ViewEvent:
		return C.eglGetDisplay(C.EGLNativeDisplayType(ve.Display))
	case app.WaylandViewEvent:
		return C.eglGetDisplay(C.EGLNativeDisplayType(ve.Display))
	}
	panic("no display available")
}

func nativeViewFor(e app.ViewEvent, size image.Point) (C.EGLNativeWindowType, func()) {
	switch e := e.(type) {
	case app.X11ViewEvent:
		return C.EGLNativeWindowType(uintptr(e.Window)), func() {}
	case app.WaylandViewEvent:
		eglWin := C.wl_egl_window_create((*C.struct_wl_surface)(e.Surface), C.int(size.X), C.int(size.Y))
		return C.EGLNativeWindowType(uintptr(unsafe.Pointer(eglWin))), func() {
			C.wl_egl_window_destroy(eglWin)
		}
	}
	panic("no native view available")
}

type (
	C = layout.Context
	D = layout.Dimensions
)

type notifyFrame int

const (
	notifyNone notifyFrame = iota
	notifyInvalidate
	notifyPrint
)

// notify keeps track of whether we want to print to stdout to notify the user
// when a frame is ready. Initially we want to notify about the first frame.
var notify = notifyInvalidate

type eglContext struct {
	disp    C.EGLDisplay
	ctx     C.EGLContext
	surf    C.EGLSurface
	cleanup func()
}

func main() {
	go func() {
		// Set CustomRenderer so we can provide our own rendering context.
		w := new(app.Window)
		w.Option(app.CustomRenderer(true))
		if err := loop(w); err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}()
	app.Main()
}

func loop(w *app.Window) error {
	var ops op.Ops
	var (
		ctx    *eglContext
		gioCtx gpu.GPU
		ve     app.ViewEvent
		init   bool
		size   image.Point
	)

	recreateContext := func() {
		w.Run(func() {
			if gioCtx != nil {
				gioCtx.Release()
				gioCtx = nil
			}
			if ctx != nil {
				C.eglMakeCurrent(ctx.disp, nil, nil, nil)
				ctx.Release()
				ctx = nil
			}
			c, err := createContext(ve, size)
			if err != nil {
				log.Fatal(err)
			}
			ctx = c
		})
		if ok := C.eglMakeCurrent(ctx.disp, ctx.surf, ctx.surf, ctx.ctx); ok != C.EGL_TRUE {
			err := fmt.Errorf("eglMakeCurrent failed (%#x)", C.eglGetError())
			log.Fatal(err)
		}
		glGetString := func(e C.GLenum) string {
			return C.GoString((*C.char)(unsafe.Pointer(C.glGetString(e))))
		}
		fmt.Printf("GL_VERSION: %s\nGL_RENDERER: %s\n", glGetString(C.GL_VERSION), glGetString(C.GL_RENDERER))
		var err error
		gioCtx, err = gpu.New(gpu.OpenGL{ES: true, Shared: true})
		if err != nil {
			log.Fatal(err)
		}
	}

	topLeft := quarterWidget{
		color: color.NRGBA{R: 0xde, G: 0xad, B: 0xbe, A: 0xff},
	}
	topRight := quarterWidget{
		color: color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff},
	}
	botLeft := quarterWidget{
		color: color.NRGBA{R: 0x00, G: 0x00, B: 0x00, A: 0xff},
	}
	botRight := quarterWidget{
		color: color.NRGBA{R: 0x00, G: 0x00, B: 0x00, A: 0x80},
	}

	// eglMakeCurrent binds a context to an operating system thread. Prevent Go from switching thread.
	runtime.LockOSThread()
	for {
		switch e := w.Event().(type) {
		case app.ViewEvent:
			ve = e
			init = true
			if size != (image.Point{}) {
				recreateContext()
			}
		case app.DestroyEvent:
			return e.Err
		case app.FrameEvent:
			if init && size != e.Size {
				size = e.Size
				recreateContext()
			}
			if gioCtx == nil || !init {
				break
			}
			// Build ops.
			gtx := app.NewContext(&ops, e)

			// Clear background to white, even on embedded platforms such as webassembly.
			paint.Fill(gtx.Ops, color.NRGBA{A: 0xff, R: 0xff, G: 0xff, B: 0xff})
			layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Flexed(1, func(gtx C) D {
					return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
						// r1c1
						layout.Flexed(1, func(gtx C) D { return topLeft.Layout(gtx) }),
						// r1c2
						layout.Flexed(1, func(gtx C) D { return topRight.Layout(gtx) }),
					)
				}),
				layout.Flexed(1, func(gtx C) D {
					return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
						// r2c1
						layout.Flexed(1, func(gtx C) D { return botLeft.Layout(gtx) }),
						// r2c2
						layout.Flexed(1, func(gtx C) D { return botRight.Layout(gtx) }),
					)
				}),
			)
			gtx.Execute(op.InvalidateCmd{})
			log.Println("frame")

			// Trigger window resize detection in ANGLE.
			C.eglWaitClient()
			// Draw custom OpenGL content.
			drawGL()

			// Render drawing ops.
			if err := gioCtx.Frame(gtx.Ops, gpu.OpenGLRenderTarget{}, e.Size); err != nil {
				log.Fatal(fmt.Errorf("render failed: %v", err))
			}

			// Process non-drawing ops.
			e.Frame(gtx.Ops)
			switch notify {
			case notifyInvalidate:
				notify = notifyPrint
				w.Invalidate()
			case notifyPrint:
				notify = notifyNone
				fmt.Println("gio frame ready")
			}

			if ok := C.eglSwapBuffers(ctx.disp, ctx.surf); ok != C.EGL_TRUE {
				log.Fatal(fmt.Errorf("swap failed: %v", C.eglGetError()))
			}

		}
	}
	return nil
}

func drawGL() {
	C.glClearColor(0, 0, 0, 1)
	C.glClear(C.GL_COLOR_BUFFER_BIT | C.GL_DEPTH_BUFFER_BIT)
}

func createContext(ve app.ViewEvent, size image.Point) (*eglContext, error) {
	view, cleanup := nativeViewFor(ve, size)
	var nilv C.EGLNativeWindowType
	if view == nilv {
		return nil, fmt.Errorf("failed creating native view")
	}
	disp := getDisplay(ve)
	if disp == 0 {
		return nil, fmt.Errorf("eglGetPlatformDisplay failed: 0x%x", C.eglGetError())
	}
	var major, minor C.EGLint
	if ok := C.eglInitialize(disp, &major, &minor); ok != C.EGL_TRUE {
		return nil, fmt.Errorf("eglInitialize failed: 0x%x", C.eglGetError())
	}
	exts := strings.Split(C.GoString(C.eglQueryString(disp, C.EGL_EXTENSIONS)), " ")
	srgb := hasExtension(exts, "EGL_KHR_gl_colorspace")
	attribs := []C.EGLint{
		C.EGL_RENDERABLE_TYPE, C.EGL_OPENGL_ES2_BIT,
		C.EGL_SURFACE_TYPE, C.EGL_WINDOW_BIT,
		C.EGL_BLUE_SIZE, 8,
		C.EGL_GREEN_SIZE, 8,
		C.EGL_RED_SIZE, 8,
		C.EGL_CONFIG_CAVEAT, C.EGL_NONE,
	}
	if srgb {
		// Some drivers need alpha for sRGB framebuffers to work.
		attribs = append(attribs, C.EGL_ALPHA_SIZE, 8)
	}
	attribs = append(attribs, C.EGL_NONE)
	var (
		cfg     C.EGLConfig
		numCfgs C.EGLint
	)
	if ok := C.eglChooseConfig(disp, &attribs[0], &cfg, 1, &numCfgs); ok != C.EGL_TRUE {
		return nil, fmt.Errorf("eglChooseConfig failed: 0x%x", C.eglGetError())
	}
	if numCfgs == 0 {
		supportsNoCfg := hasExtension(exts, "EGL_KHR_no_config_context")
		if !supportsNoCfg {
			return nil, errors.New("eglChooseConfig returned no configs")
		}
	}
	ctxAttribs := []C.EGLint{
		C.EGL_CONTEXT_CLIENT_VERSION, 3,
		C.EGL_NONE,
	}
	ctx := C.eglCreateContext(disp, cfg, nil, &ctxAttribs[0])
	if ctx == nil {
		return nil, fmt.Errorf("eglCreateContext failed: 0x%x", C.eglGetError())
	}
	var surfAttribs []C.EGLint
	if srgb {
		surfAttribs = append(surfAttribs, C.EGL_GL_COLORSPACE, C.EGL_GL_COLORSPACE_SRGB)
	}
	surfAttribs = append(surfAttribs, C.EGL_NONE)
	surf := C.eglCreateWindowSurface(disp, cfg, view, &surfAttribs[0])
	if surf == nil {
		return nil, fmt.Errorf("eglCreateWindowSurface failed (0x%x)", C.eglGetError())
	}
	return &eglContext{disp: disp, ctx: ctx, surf: surf, cleanup: cleanup}, nil
}

func (c *eglContext) Release() {
	if c.ctx != nil {
		C.eglDestroyContext(c.disp, c.ctx)
	}
	if c.surf != nil {
		C.eglDestroySurface(c.disp, c.surf)
	}
	if c.cleanup != nil {
		c.cleanup()
	}
	*c = eglContext{}
}

func hasExtension(exts []string, ext string) bool {
	for _, e := range exts {
		if ext == e {
			return true
		}
	}
	return false
}

// quarterWidget paints a quarter of the screen with one color. When clicked, it
// turns red, going back to its normal color when clicked again.
type quarterWidget struct {
	color color.NRGBA

	clicked bool
}

var red = color.NRGBA{R: 0xff, G: 0x00, B: 0x00, A: 0xff}

func (w *quarterWidget) Layout(gtx layout.Context) layout.Dimensions {
	var color color.NRGBA
	if w.clicked {
		color = red
	} else {
		color = w.color
	}

	r := image.Rectangle{Max: gtx.Constraints.Max}
	paint.FillShape(gtx.Ops, color, clip.Rect(r).Op())

	defer clip.Rect(image.Rectangle{
		Max: image.Pt(gtx.Constraints.Max.X, gtx.Constraints.Max.Y),
	}).Push(gtx.Ops).Pop()
	event.Op(gtx.Ops, w)
	for {
		e, ok := gtx.Event(pointer.Filter{
			Target: w,
			Kinds:  pointer.Press,
		})
		if !ok {
			break
		}
		if e, ok := e.(pointer.Event); ok && e.Kind == pointer.Press {
			w.clicked = !w.clicked
			// notify when we're done updating the frame.
			notify = notifyInvalidate
		}
	}
	return layout.Dimensions{Size: gtx.Constraints.Max}
}
