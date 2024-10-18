// ffox-remote sends remote control commands to a Unix Firefox instance.
// Normally this is used to have that Firefox open new URLs for you.
// This mimics what Firefox will do if you run a second copy but is much
// lighter weight and can do some things that Firefox normally won't do.
//
// usage: ffox-remote [option ...] [URL ...]
//
// The URL may be anything that Firefox recognizes, including 'about:'
// URLs. If no URL is given, Firefox will open whatever you've set as
// your default new window/new tab page. The URL doesn't have to have an
// explicit http:/https:/etc; instead, anything you give will normally
// be interpreted as a URL and handled however Firefox handles it (eg if
// you give 'fred' as an argument, Firefox will try to find fred.com).
//
// The options are:
//
//	-new-window
//	-new-tab
//		These options are passed to the running Firefox and
//		force it to open the URL(s) in new windows or new
//		tabs respectively regardless of what your settings
//		are.
//
//	-search
//		Do a search on the 'URL' arguments instead of opening
//		them as URLs, as if they were entered into Firefox's
//		address bar. -search can't be used with -new-window
//		or -new-tab (sorry, it's how Firefox behaves).
//		Mechanically, this passes -search to the running
//		Firefox and turns all arguments into a single argument
//		that Firefox will search for.
//
//	-P PROFILE
//	-U USER
//	-G PROGRAM
//		These set the name of the Firefox profile, user, and
//		program to match Firefox windows against, in case you
//		have multiple Firefox sessions running on the same X
//		server. A blank value matches anything (and if there
//		are multiple sessions, which one matches is uncertain).
//		The default settings are -P 'default' -U '' -G 'firefox',
//		which is normally what you want.
//
//	-force	Force us to talk to Firefox even if we can't get the
//		lock for the remote command protocol. This may be
//		necessary in some situations. We clear the lock if
//		this is used.
//
//	-v	Be verbose; report the Firefox window ID and Firefox's
//		response to our command.
//
//	-find	Don't send a command to Firefox, just report its window
//		ID. This is mostly useful for debugging purposes.
//
//	-pref PREFIX
//		Use PREFIX as the prefix on the Firefox X property names,
//		instead of the normal _MOZILLA. This is only really useful
//		for Chris Siebenmann.
//
// To start multiple sessions of Firefox with different profiles that
// still listen for remote commands, you need to use '-new-instance'
// when starting new instances. If you do nothing, they will try to
// remote control your existing instance (even though they're using a
// different profile); if you use -no-remote, they'll start but not
// listen for remote control commands at all. ffox-remote with -P
// can properly find and remote control each instance.
//
// Technically this passes a Firefox command line to the running Firefox,
// but I've only tested this with passing URLs so I have no idea if other
// Firefox command line options do anything useful or if they malfunction
// spectacularly. Be cautious. In particular, -private does not appear to
// work; instead it will silently fail to take effect.
//
// (A future version of ffox-remote may catch and block -private to
// protect against problems here.)
//
// ffox-remote works for modern versions of Firefox that have
// dropped support for the -remote argument and the _MOZILLA_COMMAND
// X property that it relied on (they now use a more complicated
// scheme of transmitting the nominal Firefox command line in
// _MOZILLA_COMMANDLINE). For a discussion of Firefox's current X
// property protocol for remote control, see the comment later on in
// main.go. It may not work for very old versions of Firefox that do not
// support _MOZILLA_COMMANDLINE at all.
//
// BUGS:
//
// This doesn't do what you expect:
//
//     ffox-remote -search "a thing" thing2
//
// Right now this is equivalent to '-search a thing thing2'.
//
package main

// Author: Chris Siebenmann
// https://github.com/siebenmann/ffox-remote
// Copyright: GPL v3

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	//"github.com/BurntSushi/xgb"
	"github.com/BurntSushi/xgb/xproto"
	"github.com/BurntSushi/xgbutil"
	"github.com/BurntSushi/xgbutil/xevent"
	"github.com/BurntSushi/xgbutil/xprop"
	"github.com/BurntSushi/xgbutil/xwindow"
)

// The X property names that the Firefox remote control protocol uses.
//
// These are vars instead of consts because of a gory hack for Chris's
// personal use.
var (
	lockProp = "_MOZILLA_LOCK"
	cmdlProp = "_MOZILLA_COMMANDLINE"
	respProp = "_MOZILLA_RESPONSE"
	versProp = "_MOZILLA_VERSION"
	// Mozilla user, profile (usually 'default'), and
	// program name (usually 'firefox')
	userProp = "_MOZILLA_USER"
	profProp = "_MOZILLA_PROFILE"
	progProp = "_MOZILLA_PROGRAM"
)

const (
	// Current value for versProp. This is a *protocol* version, not
	// a Firefox version.
	firefoxVersion = "5.1"
)

// FIREFOX'S REMOTE CONTROL PROTOCOL
//
// The general remote control protocol goes like this:
//
// 1. Find a or the Firefox window. It will have WM_STATE and at least
//    _MOZILLA_VERSION set on it. Make sure you think you understand
//    the protocol version; we conservatively insist on it being exactly
//    5.1.
//
// 2. Check that _MOZILLA_PROFILE, _MOZILLA_USER, and _MOZILLA_PROGRAM
//    match so that you are talking to the right instance with the right
//    profile. If you have found a Firefox window but it is the wrong
//    profile et al, continue looking (return to step 1).
//
// 3. Obtain the remote control lock by being the person to set
//    _MOZILLA_LOCK on the window. If you can't, wait for the
//    _MOZILLA_LOCK property to go away and try again.
//    (In theory the contents should be something that identify you, for
//    help in debugging. In practice this doesn't matter; who's going to
//    look?)
//    The lock is necessary to prevent two different remote control
//    clients from stomping over each other's efforts to send Firefox
//    a command and read its reply. I don't think it's needed otherwise,
//    but Firefox may look for it to be set or changed as a marker of
//    something. Someday I may find out.
//
// 4. Set _MOZILLA_COMMANDLINE to the encoded Firefox command line. See
//    the comment later on for how this is encoded, because it is crazy.
//
// 5. Wait for _MOZILLA_RESPONSE to be set and read it. In theory it is
//    a SMTP/HTTP style 'Nxx <message>' response, where a '2xx' reply is
//    success, a '5xx' is failure, a '1xx' means in progress, and there's
//    some other prefixes too. In practice current versions of Firefox
//    only ever send 200 or 5xx responses.
//
// 6. Release your ownership of _MOZILLA_LOCK by deleting the property.
//
// Note that because unlocking requires actively clearing a property,
// it's possible for a fumbled remote control attempt to leave Firefox
// in a 'locked' state. For this reason we support not trying to
// acquire the lock (and we still clear the lock).

// We use the low level X Atom values for locking and the response, so
// we look them up at the start and remember them (effectively
// interning them in the server).
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
// XmuClientWindow(), based on its documentation; we look through
// direct children of the window for one with WM_STATE set, and if
// there isn't one we return the window itself.
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

// propMatch returns true if val is empty or if the X property prop is set
// to it. It works only for string properties.
func propMatch(xu *xgbutil.XUtil, win xproto.Window, prop, val string) bool {
	pv, e := xprop.GetProperty(xu, win, prop)
	if e != nil {
		return false
	}
	// unset value matches anything
	return (val == "" || string(pv.Value) == val)
}

// As of Firefox 131 or so, the 'profile' X property value is actually
// the full path to the profile. This is also true for the D-Bus
// version of the protocol. We cope by matching a full path if you
// gave us one or only the suffix otherwise, so you can continue to
// use plain profile names.
func profileMatch(xu *xgbutil.XUtil, win xproto.Window, prop, val string) bool {
	pv, e := xprop.GetProperty(xu, win, prop)
	if e != nil {
		return false
	}
	// unset value matches anything
	sv := string(pv.Value)
	if val == "" || sv == val {
		return true
	}
	// If the property value starts with a /, we are dealing with
	// the new Firefox 131 format. If the profile value to match
	// against doesn't start with a /, assuming it is the old
	// style name and match it against the '.<name>' at the end of
	// the full profile path.
	if sv[0] == '/' && val[0] != '/' &&
		strings.HasSuffix(sv, "."+val) {
		return true
	}
	return false
}

// Find the Firefox window for a specific user, profile, and program
// (if they are set). The window must have the exact correct version.
// On failure we return 0. We print a warning if we found what looks
// like a Firefox window but it has a _MOZILLA_VERSION with the wrong
// version; this is for debugging in case the version ever does change
// again.
//
// (<jwz>'s old moz-remote.c preferred an exact match but would take
// any window with a _MOZILLA_VERSION if it had to. This is no longer
// fully viable and anyways this way is simpler code.)
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
			profileMatch(xu, win, profProp, profile) &&
			propMatch(xu, win, progProp, program) {
			return win
		}
	}
	// We only get here if we failed to find a matching window.
	// Code flow means we'll print this warning if we found both
	// a wrong-version window and a right-version window with a
	// mismatch in protocol et al.
	if wrongver != "" {
		log.Printf("found a protocol %s Firefox window but no %s one.", wrongver, firefoxVersion)
	}
	return 0
}

// waitForPropChange waits for the X property patom on window win to
// change or disappear (ie, a PropertyNotify event for it). It returns
// with the event and true if this happened; it returns with an
// undefined event and false if the window was deleted instead.
func waitForPropChange(xu *xgbutil.XUtil, win xproto.Window, patom xproto.Atom) (xevent.PropertyNotifyEvent, bool) {
	var event xevent.PropertyNotifyEvent
	good := false
	done := false
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
	// We must start listening to PropertyNotify events on the
	// target window before we start trying to lock the window,
	// because otherwise there is a race between our lock attempt
	// failing, the lock holder removing the property, and us
	// starting to listen to the event that could leave us hanging
	// with the property unlocked.
	// The ice is thin here. Let's hope this doesn't come up often.
	// (Maybe we need to start listening while having the server
	// grabbed.)
	// My approach here is at least no worse than existing code that
	// has worked for years.
	w := xwindow.New(xu, win)
	e := w.Listen(xproto.EventMaskPropertyChange, xproto.EventMaskStructureNotify)
	if e != nil {
		log.Fatal("listen error:", e)
	}

	// If we're forced, we don't try to lock Firefox but we will unlock
	// it. As a side effect this will unstick a Firefox that has been
	// locked and never unlocked.
	if !force {
		lockFirefox(xu, win)
	}

	// we can't use 'defer unlockFirefox()' because we're going
	// to call log.Fatal().
	e = xprop.ChangeProp(xu, win, 8, cmdlProp, "STRING", cmd)
	if e != nil {
		unlockFirefox(xu, win)
		log.Fatal("command line change:", e)
	}

	resp := getResponse(xu, win)
	unlockFirefox(xu, win)
	xu.Sync()
	return resp
}

// _MOZILLA_COMMANDLINE encoding
// The following comment is taken from
// toolkit/components/remote/nsXRemoteService.cpp :
//
// the commandline property is constructed as an array of int32_t
// followed by a series of null-terminated strings:
//
// [argc][offsetargv0][offsetargv1...]<workingdir>\0<argv[0]>\0argv[1]...\0
// (offset is from the beginning of the buffer)
//
// ---
// Although not documented, the integers are little-endian.
// In practice the pwd is ignored by Firefox right now (from what I can
// tell).

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

// Rewrite all of our property names to have a different prefix.
// This is a gory hack to keep the rest of the code simple because
// Chris can't think of a better way right now.
func fixupPref(pfix string, elems ...*string) {
	plen := len("_MOZILLA")
	for _, e := range elems {
		us := *e
		ns := fmt.Sprintf("%s%s", pfix, us[plen:])
		*e = ns
	}
}

func main() {
	// Set Unix-like logging: to stderr, no timestamps, and our program
	// name as a prefix.
	log.SetPrefix("ffox-remote: ")
	log.SetFlags(0)

	user := flag.String("U", "", "Firefox user to match against")
	profile := flag.String("P", "default", "Firefox profile to match against")
	program := flag.String("G", "firefox", "Firefox program name to match against")
	force := flag.Bool("force", false, "Force us to go on even without the X window lock")
	pfix := flag.String("pref", "", "Non-default X property prefix (hack)")
	find := flag.Bool("find", false, "Find the Firefox window and exit")
	verb := flag.Bool("v", false, "extra verbosity")
	// In theory we could make users type 'ffox-remote ... -- -new-window'
	// in order to have -new-window and -new-tab be passed to Firefox.
	// In practice that is user-hostile, so we accept them as arguments
	// that pass through.
	nw := flag.Bool("new-window", false, "Pass -new-window to Firefox")
	nt := flag.Bool("new-tab", false, "Pass -new-tab to Firefox")
	search := flag.Bool("search", false, "Pass -search to Firefox to do a search")

	flag.Parse()

	// This is a gory hack. Don't ask.
	if *pfix != "" {
		fixupPref(*pfix, &lockProp, &cmdlProp, &respProp, &versProp, &userProp, &profProp, &progProp)
	}

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
	count := 0
	if *nw {
		args = append(args, "-new-window")
		count++
	}
	if *nt {
		args = append(args, "-new-tab")
		count++
	}
	if *search {
		args = append(args, "-search")
		count++
	}
	if count > 1 {
		log.Fatal("conflicting arguments:", strings.Join(args[1:], " "))
	}

	cwd, e := os.Getwd()
	if e != nil {
		log.Print("cannot get current directory:", e)
		cwd = "/"
	}
	// If we are given -search we do the convenient thing by
	// turning all of the rest of the arguments into a single
	// search term. Otherwise Firefox searches for the first
	// argument and opens the rest of them as URLs, which is
	// not really what you generally want.
	if *search {
		args = append(args, strings.Join(flag.Args(), " "))
	} else {
		args = append(args, flag.Args()...)
	}
	enc := encodeCommandLine(cwd, args)

	resp := submitCommand(xu, foxwin, enc, *force)
	if *verb {
		fmt.Printf("response: %s\n", resp)
	}
}
