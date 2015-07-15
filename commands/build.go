// Copyright (c) 2015 Pagoda Box Inc
//
// This Source Code Form is subject to the terms of the Mozilla Public License, v.
// 2.0. If a copy of the MPL was not distributed with this file, You can obtain one
// at http://mozilla.org/MPL/2.0/.
//

package commands

import (
	"encoding/json"
	"flag"
	"fmt"
	// "os"

	"github.com/pagodabox/golang-mist"
	api "github.com/pagodabox/nanobox-api-client"
	"github.com/pagodabox/nanobox-cli/config"
	"github.com/pagodabox/nanobox-cli/ui"
	"github.com/pagodabox/nanobox-golang-stylish"
)

type (

	// BuildCommand satisfies the Command interface for deploying to nanobox
	BuildCommand struct{}

	// Build
	Build struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
)

// Help prints detailed help text for the app list command
func (c *BuildCommand) Help() {
	ui.CPrint(`
Description:
  Issues a build to your nanobox

Usage:
  nanobox build
  nanobox build -v
  nanobox build -r

Options:
  -v, --verbose
    Increase the level of log output from 'info' to 'debug'

  -r, --reset
    Clears cached libraries the project might use
  `)
}

// Run issues a build to the running nanobox VM
func (c *BuildCommand) Run(opts []string) {

	// flags
	flags := flag.NewFlagSet("flags", flag.ContinueOnError)
	flags.Usage = func() { c.Help() }

	//
	var fReset bool
	flags.BoolVar(&fReset, "r", false, "")
	flags.BoolVar(&fReset, "reset", false, "")

	// the verbose flag allows a user to request verbose output during a build. The
	// default is false
	var fVerbose bool
	flags.BoolVar(&fVerbose, "v", false, "")
	flags.BoolVar(&fVerbose, "verbose", false, "")

	//
	if err := flags.Parse(opts); err != nil {
		ui.LogFatal("[commands.destroy] flags.Parse() failed", err)
	}

	// start the vm if it's not already running
	// resume := ResumeCommand{}
	// resume.Run(opts)

	// create a 'mist' client to communicate with the mist server running on the
	// guest machine
	client := mist.Client{Host: config.Boxfile.IP, Port: "1445"}

	//
	// connect the 'mist' client to the 'mist' server
	if err := client.Connect(); err != nil {
		ui.LogFatal("[commands build] client.Connect() failed ", err)
	}
	defer client.Close()

	printv(stylish.Bullet("Subscribing to mist..."), fVerbose)

	// subscribe to 'build' updates
	printv("\n   - Subscribing to app logs", fVerbose)
	buildSub, err := client.Subscribe([]string{"job", "build"})
	if err != nil {
		fmt.Printf(stylish.Warning("Nanobox failed to subscribe to app logs. Your build will continue as normal, and log output is available on your dashboard."))
	}
	defer client.Unsubscribe(buildSub)

	// subscribe to the 'build' logs
	printv("\n   - Subscribing to info logs", fVerbose)
	infoSub, err := client.Subscribe([]string{"log", "build", "info"})
	if err != nil {
		fmt.Printf(stylish.Warning("Nanobox failed to subscribe to info logs. Your build will continue as normal, and log output is available on your dashboard."))
	}
	defer client.Unsubscribe(infoSub)

	// if the verbose flag is included, also subscribe to the 'debug' build logs
	if fVerbose {
		printv("\n   - Subscribing to debug logs", fVerbose)
		debugSub, err := client.Subscribe([]string{"log", "build", "debug"})
		if err != nil {
			fmt.Printf(stylish.Warning("Nanobox failed to subscribe to debug logs. Your build will continue as normal, and log output is available on your dashboard."))
		}
		defer client.Unsubscribe(debugSub)
	}

	printv("   [√] SUCCESS\n", fVerbose)

	//
	// issue a build

	path := fmt.Sprintf("http://%v:1757/builds", config.Boxfile.IP)

	// if the reset flag is passed append a "reset=true" query string param
	if fReset {
		path += "?reset=true"
	}

	//
	if err := api.DoRawRequest(nil, "POST", path, nil, nil); err != nil {
		ui.LogFatal("[commands build] api.DoRawRequest() failed ", err)
	}

	//
	entry := &Entry{}

	// listen for messages coming from mist
stream:
	for msg := range client.Data {

		// check for any error message that cause mist to disconnect, and release
		// nanobox
		if msg.Tags[0] == "err" {
			fmt.Printf(stylish.Warning("An unexpected error caused the build stream to disconnect. Your build should continue as normal, and you can see the log output from your dashboard."))
			break stream
		}

		//
		if err := json.Unmarshal([]byte(msg.Data), &entry); err != nil {
			ui.LogFatal("[commands build] json.Unmarshal() failed", err)
		}

		// depending on what fields the data has, determines what needs to happen...
		switch {

		// if the message contains the log field, the log is printed. The message is
		// then checked to see if it contains a model field...
		// example entry: {Time: "time", Log: "content"}
		case entry.Log != "":
			fmt.Printf(entry.Log)
			fallthrough

		// if the message contains the model field...
		case entry.Model != "":

			// depending on the type of model, different things may happen...
			switch entry.Model {

			// in the case of a build model, listen for a complete to close the stream
			case "Build", "build":

				build := &Build{}

				if err := json.Unmarshal([]byte(entry.Document), build); err != nil {
					ui.LogFatal("[commands build] json.Unmarshal() failed ", err)
				}

				switch build.Status {
				// once the build is 'complete' unsubscribe from mist
				case "compelete":
					fmt.Printf(stylish.Bullet(fmt.Sprintf("Build complete... Navigate to %v.nano.dev to view your app.", config.App)))
					break stream

				// if the build is 'errored' unsubscribe from mist
				case "errored":
					fmt.Printf(stylish.Error("build failed", "Your build failed to... well... build..."))
					break stream
				}

			// report any unhandled models, incase cases need to be added to handle them
			case "default":
				config.Console.Debug("Nanobox has encountered an unknown model (%v), and doesn't know what to do with it...", entry.Model)
				break stream
			}

		// report any unhandled entries, incase cases need to be added to handle them
		default:
			config.Console.Debug("Nanobox has encountered an Entry with neither a 'log' nor 'model' field and doesnt know what to do...")
			break stream
		}
	}
}
