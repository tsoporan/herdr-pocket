// Command fake-herdr serves herdr's socket protocol from the built-in studio
// fixture (the mockup's nine agents), optionally replaying a script of timed
// changes. It never listens on the real herdr socket.
//
//	fake-herdr --socket /tmp/fake-herdr.sock [--script internal/fakeherdr/testdata/demo-script.json] [--protocol 22] [--turn 20s]
//	           [--transcripts internal/fakeherdr/testdata/claude --transcripts-out DIR]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/fakeherdr"
	"github.com/tsoporan/herdr-pocket/internal/herdr"
)

func main() {
	socket := flag.String("socket", "", "unix socket to listen on (required; never the real herdr socket)")
	script := flag.String("script", "", "JSON script of timed changes (see fakeherdr.Step)")
	protocol := flag.Int("protocol", herdr.MinProtocol, "protocol version to report in ping")
	turn := flag.Duration("turn", 0, "how long a prompted agent works before it goes idle by itself (0: until the script changes it)")
	transcripts := flag.String("transcripts", "", "fixture Claude Code config dir to copy to --transcripts-out with its times moved up to now")
	transcriptsOut := flag.String("transcripts-out", "", "where to write that copy (replaced)")
	flag.Parse()
	log.SetFlags(log.Ltime)
	if *transcripts != "" {
		if *transcriptsOut == "" {
			log.Fatal("fake-herdr: --transcripts needs --transcripts-out")
		}
		if err := fakeherdr.FreshTranscripts(*transcripts, *transcriptsOut, time.Now()); err != nil {
			log.Fatal(err)
		}
	}
	if err := run(*socket, *script, *protocol, *turn); err != nil {
		log.Fatal(err)
	}
}

func run(socket, script string, protocol int, turn time.Duration) error {
	if socket == "" {
		return fmt.Errorf("fake-herdr: --socket is required")
	}
	var steps []fakeherdr.Step
	if script != "" {
		data, err := os.ReadFile(script)
		if err != nil {
			return err
		}
		if steps, err = fakeherdr.ParseScript(data); err != nil {
			return err
		}
	}

	fx := fakeherdr.Studio()
	srv, err := fakeherdr.Listen(socket, fx)
	if err != nil {
		return err
	}
	defer srv.Close()
	srv.SetProtocol(protocol)
	srv.SetTurn(turn)
	srv.OnInput(func(in fakeherdr.Input) {
		switch in.Method {
		case "tab.create":
			log.Printf("input %s %s label=%q cwd=%q params=%s", in.Method, in.PaneID, in.Label, in.Cwd, in.Params)
		case "agent.start":
			log.Printf("input %s %s name=%q kind=%q params=%s", in.Method, in.PaneID, in.Name, in.Kind, in.Params)
		default:
			log.Printf("input %s %s keys=%q text=%q name=%q", in.Method, in.PaneID, in.Keys, in.Text, in.Name)
		}
	})
	log.Printf("fake-herdr listening on %s: %d agents, protocol %d", srv.Addr(), len(fx.Agents), protocol)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(steps) > 0 {
		go func() {
			err := srv.Run(ctx, steps, func(st fakeherdr.Step) {
				if st.PaneID == "" {
					delay := "unchanged"
					if st.StartDelay != nil {
						delay = time.Duration(*st.StartDelay).String()
					}
					log.Printf("script +%s: agent.start kind=%q start=%q delay=%s", time.Duration(st.After), st.Kind, st.Start, delay)
					return
				}
				log.Printf("script +%s: %s status=%q title=%q focus=%t", time.Duration(st.After), st.PaneID, st.Status, st.Title, st.Focus)
			})
			if err != nil && ctx.Err() == nil {
				log.Printf("script stopped: %v", err)
			}
		}()
	}
	<-ctx.Done()
	log.Print("fake-herdr stopping")
	return nil
}
