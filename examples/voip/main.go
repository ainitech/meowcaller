// Command voip is a cross-platform CLI demo of the meowcaller calling layer.
//
//	voip loopback        Mic → MLow → E2E-SRTP protect/unprotect → MLow → speaker
//	                     (no WhatsApp; exercises the whole media stack on real audio).
//	voip call <target>   Log in, resolve the peer LID, discover devices, and send a
//	                     <call><offer> (target = phone number, phone JID, or @lid JID).
//	voip listen          Log in and print incoming call signaling.
//	voip autoaccept      Log in and auto-accept incoming calls (decrypt callKey,
//	                     reply preaccept + accept).
//
// Audio is captured/played via miniaudio (malgo), so it runs on macOS, Linux and
// Windows with the OS default mic and speaker. WhatsApp metrics (WAM) are reported
// while connected so the session looks like a real client.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"
)

func main() {
	// As the top-level program, this command owns logger configuration (the library
	// packages only accept a logger from the context). A console writer keeps the demo
	// readable; the logger is embedded in the context so every callee resolves it with
	// zerolog.Ctx(ctx).
	logger := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05.000"}).
		Level(zerolog.DebugLevel).
		With().Timestamp().Logger()

	if len(os.Args) < 2 {
		usage()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx = logger.WithContext(ctx)

	var err error
	switch os.Args[1] {
	case "loopback":
		err = runLoopback(ctx)
	case "call":
		if len(os.Args) < 3 {
			usage()
		}
		err = runCall(ctx, os.Args[2])
	case "listen":
		accept := len(os.Args) > 2 && os.Args[2] == "accept"
		err = runListen(ctx, accept)
	default:
		usage()
	}
	if err != nil {
		logger.Fatal().Err(err).Str("command", os.Args[1]).Msg("voip command failed")
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: voip <loopback | call <target> | listen [accept]>")
	os.Exit(2)
}
