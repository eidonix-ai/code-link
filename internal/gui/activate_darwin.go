//go:build darwin

package gui

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
#import <Cocoa/Cocoa.h>

static void activateApp(void) {
	dispatch_async(dispatch_get_main_queue(), ^{
		[NSApp activateIgnoringOtherApps:YES];
	});
}
*/
import "C"

// activate brings code-link to the front, so a dialog shown from the menu
// bar isn't hidden behind other apps' windows.
func activate() { C.activateApp() }
