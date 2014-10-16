// ffox-remote issues remote commands to Firefox through X windows properties
//
// usage: ffox-remote [-U user] [-P profile] [-G program] [-find] [-v] [-new-window|-new-tab] [URL ...]
//
//   -U/-P/-G: set options used to match a specific Firefox window.
//             Normally -P is 'default', -G is 'firefox', and -U is blank
//             to match anything.
//   -find: only find the Firefox window and report its ID.
//   -v: be more verbose. Report Firefox response and window ID.
//   -new-window|-new-tab: passed to Firefox to open the URL(s) in new
//                         windows and/or tabs.
//
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"

	//"github.com/BurntSushi/xgb"
	"github.com/BurntSushi/xgb/xproto"
	"github.com/BurntSushi/xgbutil"
	"github.com/BurntSushi/xgbutil/xevent"
	"github.com/BurntSushi/xgbutil/xprop"
	"github.com/BurntSushi/xgbutil/xwindow"
)

// The X property names that the Firefox remote control protocol uses.
const (
	lockProp = "_MEZILLA_LOCK"
	cmdlProp = "_MEZILLA_COMMANDLINE"
	respProp = "_MEZILLA_RESPONSE"
	// version is '5.1' currently and must match exactly
	versProp = "_MEZILLA_VERSION"
	// Mozilla user, profile (usually 'default'), and
	// program name (usually 'firefox')
	userProp = "_MEZILLA_USER"
	profProp = "_MEZILLA_PROFILE"
	progProp = "_MEZILLA_PROGRAM"
)

var lockatom, responseatom xproto.Atom

func getAtom(xu *xgbutil.XUtil, aname string) xproto.Atom {
	r, e := xprop.Atm(xu, aname)
	if e != nil {
		log.Fatalf("getAtom: %s", e)
	}
	return r
}

func getAtoms(xu *xgbutil.XUtil) {
	lockatom = getAtom(xu, lockProp)
	responseatom = getAtom(xu, respProp)
}

// ClientWindow finds the actual client window underneath what may be
// a window manager frame.
func ClientWindow(xu *xgbutil.XUtil, win xproto.Window) xproto.Window {
	tree, err := xproto.QueryTree(xu.Conn(), win).Reply()
	if err != nil {
		log.Fatalf("c_w: %s", err)
	}
	for _, c := range tree.Children {
		_, e := xprop.GetProperty(xu, c, "WM_STATE")
		if e == nil {
			return c
		}
	}
	// whatever, man.
	return win
}

func propMatch(xu *xgbutil.XUtil, win xproto.Window, prop, val string) bool {
	pv, e := xprop.GetProperty(xu, win, prop)
	if e != nil {
		return false
	}
	// unset value matches anything
	return (val == "" || string(pv.Value) == val)
}

// Find the Firefox window for a specific user, profile, and program if
// they are set.
func findFirefox(xu *xgbutil.XUtil, user, profile, program string) xproto.Window {
	root := xu.RootWin()

	tree, err := xproto.QueryTree(xu.Conn(), root).Reply()
	if err != nil {
		log.Fatal(err)
	}

	for _, c := range tree.Children {
		win := ClientWindow(xu, c)
		if propMatch(xu, win, versProp, "5.1") &&
			propMatch(xu, win, userProp, user) &&
			propMatch(xu, win, profProp, profile) &&
			propMatch(xu, win, progProp, program) {
			return win
		}
	}
	return 0
}

func tryLock(xu *xgbutil.XUtil, win xproto.Window) bool {
	success := false
	xu.Grab()
	p, e := xprop.GetProperty(xu, win, lockProp)
	if e != nil || len(p.Value) == 0 {
		e = xprop.ChangeProp(xu, win, 8, lockProp, "STRING",
			[]byte("ffox-remote.go on somewhere"))
		success = (e == nil)
	}
	xu.Ungrab()
	xu.Sync()
	return success
}
func lockFirefox(xu *xgbutil.XUtil, win xproto.Window) {
	res := tryLock(xu, win)
	// TODO: deal with us failing to lock Firefox.
	if !res {
		log.Fatal("failed to lock Firefox")
	}
}
func unlockFirefox(xu *xgbutil.XUtil, win xproto.Window) {
	// xproto does not expose the synchronous delete property of
	// XGetWindowProperty(), so we assume that we are the owner
	// and our ownership has not been overwritten.
	_ = xproto.DeleteProperty(xu.Conn(), win, lockatom)
}

func getResponse(xu *xgbutil.XUtil, win xproto.Window) string {
	done := false
	var resp string

	// manufacture an xwindow.Window so we can call w.Listen() to make
	// my life easier.
	w := xwindow.New(xu, win)
	e := w.Listen(xproto.EventMaskPropertyChange, xproto.EventMaskStructureNotify)
	if e != nil {
		log.Fatal("listen error:", e)
	}
	xevent.PropertyNotifyFun(
		func(xu *xgbutil.XUtil, ev xevent.PropertyNotifyEvent) {
			if ev.Atom != responseatom {
				return
			}
			p, r := xprop.GetProperty(xu, win, respProp)
			if r == nil {
				resp = string(p.Value)
			}
			// We cannot set this before we've called
			// GetProperty() because lol goroutines.
			done = true
			xevent.Quit(xu)
		}).Connect(xu, win)
	xevent.DestroyNotifyFun(
		func(xu *xgbutil.XUtil, ev xevent.DestroyNotifyEvent) {
			done = true
			xevent.Quit(xu)
		}).Connect(xu, win)

	// xevent.Read() doesn't do what you think it does. Sigh.
	bchan, achan, qchan := xevent.MainPing(xu)
	for !done {
		select {
		case <-bchan:
			// do nothing.
		case <-achan:
			// do nothing
		case <-qchan:
			// Just to be sure.
			done = true
		}
	}
	xevent.Detach(xu, win)
	xevent.Quit(xu) // just to be sure again
	return resp
}

func sendCommand(xu *xgbutil.XUtil, win xproto.Window, cmd []byte) string {
	lockFirefox(xu, win)
	// we can't use 'defer unlockFirefox()' because we're going
	// to call log.Fatal().
	e := xprop.ChangeProp(xu, win, 8, cmdlProp, "STRING", cmd)
	if e != nil {
		unlockFirefox(xu, win)
		log.Fatal("command line change:", e)
	}
	resp := getResponse(xu, win)
	unlockFirefox(xu, win)
	xu.Sync()
	return resp
}

// from toolkit/components/remote/nsXRemoteService.cpp :
// the commandline property is constructed as an array of int32_t
// followed by a series of null-terminated strings:
//
// [argc][offsetargv0][offsetargv1...]<workingdir>\0<argv[0]>\0argv[1]...\0
// (offset is from the beginning of the buffer)
//
// Although not documented, the integers are little-endian.
// In practice the pwd is ignored.

func writeOne(w io.Writer, s string) int {
	n, e := w.Write([]byte(s))
	if e != nil {
		log.Fatal("encoding", e)
	}
	n2, e := w.Write([]byte{0})
	if e != nil {
		log.Fatal("encoding 0", e)
	}
	return n + n2
}

func encodeCommandLine(pwd string, args []string) []byte {
	buf := new(bytes.Buffer)
	arr := make([]uint32, len(args)+1)
	arr[0] = uint32(len(args))
	arenc := new(bytes.Buffer)

	pos := len(arr) * 4
	pos += writeOne(arenc, pwd)
	for i := range args {
		arr[i+1] = uint32(pos)
		pos += writeOne(arenc, args[i])
	}
	e := binary.Write(buf, binary.LittleEndian, arr)
	if e != nil {
		log.Fatal("encode arrray", e)
	}
	_, e = buf.Write(arenc.Bytes())
	if e != nil {
		log.Fatal("encode add arguments", e)
	}
	return buf.Bytes()
}

func main() {
	user := flag.String("U", "", "Firefox user")
	profile := flag.String("P", "default", "Firefox profile")
	program := flag.String("G", "firefox", "Firefox program name")
	find := flag.Bool("find", false, "Find the Firefox window and exit")
	verb := flag.Bool("v", false, "extra verbosity")
	nw := flag.Bool("new-window", false, "Pass -new-window to Firefox")
	nt := flag.Bool("new-tab", false, "Pass -new-tab to Firefox")

	flag.Parse()

	xu, err := xgbutil.NewConn()
	if err != nil {
		log.Fatal(err)
	}
	getAtoms(xu)
	foxwin := findFirefox(xu, *user, *profile, *program)
	if foxwin == 0 {
		log.Fatal("Not running")
	}
	if *find || *verb {
		fmt.Printf("firefox window: 0x%x\n", foxwin)
		if *find {
			return
		}
	}

	args := []string{"firefox"}
	if *nw {
		args = append(args, "-new-window")
	}
	if *nt {
		args = append(args, "-new-tab")
	}

	args = append(args, flag.Args()...)
	enc := encodeCommandLine("/", args)

	resp := sendCommand(xu, foxwin, enc)
	if *verb {
		fmt.Printf("response: %s\n", resp)
	}
}
