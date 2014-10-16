// ffox-remote issues remote commands to Firefox through X windows
// properties, for modern versions of Firefox that have dropped
// support for the -remote argument and the _MOZILLA_COMMAND X
// property that it relied on (they now use a more complicated scheme
// of transmitting the nominal Firefox command line in
// _MOZILLA_COMMANDLINE).
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
//   -force: force us to talk to Firefox even if we can't do the magic
//           Firefox X property locking protocol. Clears this lock as
//           a (useful) side effect.
//
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	//"github.com/BurntSushi/xgb"
	"github.com/BurntSushi/xgb/xproto"
	"github.com/BurntSushi/xgbutil"
	"github.com/BurntSushi/xgbutil/xevent"
	"github.com/BurntSushi/xgbutil/xprop"
	"github.com/BurntSushi/xgbutil/xwindow"
)

// The X property names that the Firefox remote control protocol uses.
//
// The _MEZILLA_ prefix is Chris Siebenmann's own hack. The official
// protocol uses _MOZILLA_.
// TODO: figure out how to support both in one binary.
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

	// Current value for versProp. This is a *protocol* version, not
	// a Firefox version.
	firefoxVersion = "5.1"
)

// We use the low level X Atom values for locking and the response,
// so we get them now (effectively interning them in the server).
var lockatom, responseatom xproto.Atom

func getAtom(xu *xgbutil.XUtil, aname string) xproto.Atom {
	r, e := xprop.Atm(xu, aname)
	if e != nil {
		log.Fatal("getAtom:", e)
	}
	return r
}

func getAtoms(xu *xgbutil.XUtil) {
	lockatom = getAtom(xu, lockProp)
	responseatom = getAtom(xu, respProp)
}

// ClientWindow finds the actual client window underneath what may be
// a window manager frame. This is an implementation of
// XmuClientWindow().
func ClientWindow(xu *xgbutil.XUtil, win xproto.Window) xproto.Window {
	tree, err := xproto.QueryTree(xu.Conn(), win).Reply()
	if err != nil {
		log.Fatal("c_w:", err)
	}
	for _, c := range tree.Children {
		_, e := xprop.GetProperty(xu, c, "WM_STATE")
		if e == nil {
			return c
		}
	}
	// whatever, man. we'll just return the original window as the
	// best we can do.
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

// Find the Firefox window for a specific user, profile, and program
// (if they are set). The window must have the exact correct version.
//
func findFirefox(xu *xgbutil.XUtil, user, profile, program string) xproto.Window {
	var wrongver string
	root := xu.RootWin()

	// Find all children of the root window, which nominally will
	// contain the Firefox window we are looking for.
	tree, err := xproto.QueryTree(xu.Conn(), root).Reply()
	if err != nil {
		log.Fatal(err)
	}

	for _, c := range tree.Children {
		win := ClientWindow(xu, c)
		pv, err := xprop.GetProperty(xu, win, versProp)
		if err != nil {
			continue
		}
		if string(pv.Value) != firefoxVersion {
			wrongver = string(pv.Value)
			continue
		}
		if propMatch(xu, win, userProp, user) &&
			propMatch(xu, win, profProp, profile) &&
			propMatch(xu, win, progProp, program) {
			return win
		}
	}
	if wrongver != "" {
		log.Printf("found a protocol %s Firefox window but no %s one.", wrongver, firefoxVersion)
	}
	return 0
}

func waitForPropChange(xu *xgbutil.XUtil, win xproto.Window, patom xproto.Atom) (xevent.PropertyNotifyEvent, bool) {
	var event xevent.PropertyNotifyEvent
	good := false
	done := false
	w := xwindow.New(xu, win)
	e := w.Listen(xproto.EventMaskPropertyChange, xproto.EventMaskStructureNotify)
	if e != nil {
		log.Print("listen error:", e)
		return event, false
	}
	// NOTE: these two are type casts, not function calls, because we
	// have anonymous closures here.
	xevent.PropertyNotifyFun(
		func(xu *xgbutil.XUtil, ev xevent.PropertyNotifyEvent) {
			if ev.Atom != patom {
				return
			}
			event = ev
			good = true
			done = true
			xevent.Quit(xu)
		}).Connect(xu, win)
	xevent.DestroyNotifyFun(
		func(xu *xgbutil.XUtil, ev xevent.DestroyNotifyEvent) {
			done = true
			xevent.Quit(xu)
		}).Connect(xu, win)

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

	// Stop listening for property events for now.
	e = w.Listen(xproto.EventMaskStructureNotify)
	if e != nil {
		log.Print("delisten error:", e)
	}

	return event, good
}

// tryLock makes one attempt to obtain the magic Firefox lock property.
// The protocol is that lockProp normally does not exist and you take
// the lock by setting it. This must be done with the X server grabbed
// so that no one else can do that at the same time.
func tryLock(xu *xgbutil.XUtil, win xproto.Window) bool {
	success := false
	xu.Grab()
	p, e := xprop.GetProperty(xu, win, lockProp)
	if e != nil || len(p.Value) == 0 {
		// In theory we should be informative here with the
		// value we set. In practice there is no particular
		// point; you have to go well out of your way to even
		// see this property and advanced users might as well
		// use -force to override a broken lock.
		e = xprop.ChangeProp(xu, win, 8, lockProp, "STRING",
			[]byte("ffox-remote.go on somewhere"))
		success = (e == nil)
	}
	xu.Ungrab()
	xu.Sync()
	return success
}

// lockFirefox obtains the remote command invocation lock on the Firefox
// window.
// TODO: this should have a timeout. But then we'd need an X event
// timeout. Simpler to punt.
func lockFirefox(xu *xgbutil.XUtil, win xproto.Window) {
	for {
		res := tryLock(xu, win)
		if res {
			return
		}
		// Someone else has the property active. Wait for a
		// property change on it.
		_, good := waitForPropChange(xu, win, lockatom)
		if !good {
			log.Fatal("Firefox window disappeared")
		}
		// We don't bother checking the event state for
		// PropertyDelete, because we don't care. If the
		// property just changed value, we'll find out
		// when we fail to get the lock.
	}
}

// unlockFirefox unconditionally releases the remote command invocation
// lock on the Firefox window. We are assumed to own it since we have
// no simple choice.
func unlockFirefox(xu *xgbutil.XUtil, win xproto.Window) {
	// xproto does not expose the synchronous delete property of
	// XGetWindowProperty(), so we assume that we are the owner
	// and our ownership has not been overwritten.
	_ = xproto.DeleteProperty(xu.Conn(), win, lockatom)
}

// getResponse gets the response to our Firefox remote command, which
// appears in the value of respProp. We return "" if there is some
// problem.
// In theory a response starting with '1' is a 'things are in progress'
// response. In practice modern versions of Firefox never emit this in
// the first place and we don't really care anyways.
func getResponse(xu *xgbutil.XUtil, win xproto.Window) string {
	event, good := waitForPropChange(xu, win, responseatom)
	if !good || event.State != xproto.PropertyNewValue {
		return ""
	}
	p, r := xprop.GetProperty(xu, win, respProp)
	if r == nil {
		return string(p.Value)
	}
	return ""
}

// submitCommand sends our command to the remote Firefox window and
// waits for its response, returning the response string.
// We are given the already-encoded commandline property value.
// Process: obtain lock, set cmdlProp to the value, wait for the response
// property to be set (or the window to poof), unlock Firefox.
func submitCommand(xu *xgbutil.XUtil, win xproto.Window, cmd []byte, force bool) string {
	// If we're forced, we don't try to lock Firefox but we will unlock
	// it. As a side effect this will unstick a Firefox that has been
	// incorrectly locked.
	if !force {
		lockFirefox(xu, win)
	}

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

// addArgStr appends an argument to the argument buffer, returning its
// length plus the trailing 0 byte.
func addArgStr(w io.Writer, s string) int {
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

// encodeCommandLine encodes a command line as summarized above.
// We encode in two passes. In the first pass we create a string
// of all of the arguments and set up the array of offsets. In
// the second pass we encode the offsets themselves and concatenate
// the encoded argument string on the end.
func encodeCommandLine(pwd string, args []string) []byte {
	buf := new(bytes.Buffer)
	arenc := new(bytes.Buffer)

	arr := make([]uint32, len(args)+1)
	// arr[0] is argc. arr[i > 0] is the offset of args[i-1] in
	// the argument string.
	arr[0] = uint32(len(args))

	// the arr argument position array takes up four bytes per
	// element, so this is the initial offset of the start of the
	// argument strings.
	off := len(arr) * 4

	// build the argument string, remembering our running offset.
	// The working directory does not appear in the array, but it
	// has to be encoded anyways.
	off += addArgStr(arenc, pwd)
	for i := range args {
		arr[i+1] = uint32(off)
		off += addArgStr(arenc, args[i])
	}

	// Build the final result with the little endian encoded arr
	// on the front and then the argument strings.
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
	// Set Unix-like logging: to stderr, no timestamps, and our program
	// name as a prefix.
	log.SetPrefix("ffox-remote: ")
	log.SetOutput(os.Stderr)
	log.SetFlags(0)

	user := flag.String("U", "", "Firefox user to match against")
	profile := flag.String("P", "default", "Firefox profile to match against")
	program := flag.String("G", "firefox", "Firefox program name to match against")
	force := flag.Bool("force", false, "Force us to go on even without the X window lock")
	find := flag.Bool("find", false, "Find the Firefox window and exit")
	verb := flag.Bool("v", false, "extra verbosity")
	nw := flag.Bool("new-window", false, "Pass -new-window to Firefox")
	nt := flag.Bool("new-tab", false, "Pass -new-tab to Firefox")

	flag.Parse()

	xu, err := xgbutil.NewConn()
	if err != nil {
		log.Fatal("X connection:", err)
	}
	getAtoms(xu)

	// Locate the command window (or a command window) for the running
	// Firefox.
	foxwin := findFirefox(xu, *user, *profile, *program)
	if foxwin == 0 {
		log.Fatal("can't find a running Firefox window.")
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

	cwd, e := os.Getwd()
	if e != nil {
		log.Print("cannot get current directory:", e)
		cwd = "/"
	}
	args = append(args, flag.Args()...)
	enc := encodeCommandLine(cwd, args)

	resp := submitCommand(xu, foxwin, enc, *force)
	if *verb {
		fmt.Printf("response: %s\n", resp)
	}
}
