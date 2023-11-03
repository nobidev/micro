package main

import (
	"fmt"
	"github.com/go-errors/errors"
	"github.com/mattn/go-isatty"
	"github.com/urfave/cli/v2"
	lua "github.com/yuin/gopher-lua"
	"github.com/zyedidia/micro/v2/internal/action"
	"github.com/zyedidia/micro/v2/internal/buffer"
	"github.com/zyedidia/micro/v2/internal/clipboard"
	"github.com/zyedidia/micro/v2/internal/config"
	ulua "github.com/zyedidia/micro/v2/internal/lua"
	"github.com/zyedidia/micro/v2/internal/screen"
	"github.com/zyedidia/micro/v2/internal/shell"
	"github.com/zyedidia/micro/v2/internal/util"
	"github.com/zyedidia/tcell/v2"
	"io"
	"log"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"syscall"
	"time"
)

var (
	optionsFlags map[string]cli.Flag
	sigterm      chan os.Signal
	sighup       chan os.Signal
)

var app = &cli.App{
	Name:                 "micro",
	UsageText:            "[OPTIONS] [FILE]...",
	EnableBashCompletion: true,
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:  "clean",
			Usage: "Clean configuration directory",
		},
		&cli.StringFlag{
			Name:  "config-dir",
			Usage: "Specify a custom location for the configuration directory",
		},
	},
	Commands: []*cli.Command{
		{
			Name:  "version",
			Usage: "Show the version number and information",
			Action: func(ctx *cli.Context) error {
				fmt.Println("Version:", util.Version)
				fmt.Println("Commit hash:", util.CommitHash)
				fmt.Println("Compiled on", util.CompileDate)
				return nil
			},
		},
		{
			Name:  "options",
			Usage: "Show all option help",
			Action: func(ctx *cli.Context) error {
				var keys []string
				m := config.DefaultAllSettings()
				for k := range m {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					v := m[k]
					fmt.Printf("-%s value\n", k)
					fmt.Printf("    \tDefault value: '%v'\n", v)
				}
				return nil
			},
		},
	},
	Action: func(ctx *cli.Context) error {
		err := config.InitConfigDir(ctx.String("config-dir"))
		if err != nil {
			screen.TermMessage(err)
		}

		config.InitRuntimeFiles()
		err = config.ReadSettings()
		if err != nil {
			screen.TermMessage(err)
		}
		err = config.InitGlobalSettings()
		if err != nil {
			screen.TermMessage(err)
		}

		// flag options
		for k, f := range optionsFlags {
			if f.IsSet() {
				nativeValue, err := config.GetNativeValue(k, config.DefaultAllSettings()[k], f.String())
				if err != nil {
					screen.TermMessage(err)
					continue
				}
				config.GlobalSettings[k] = nativeValue
			}
		}

		err = screen.Init()
		if err != nil {
			fmt.Println(err)
			fmt.Println("Fatal: Micro could not initialize a Screen.")
			os.Exit(1)
		}
		m := clipboard.SetMethod(config.GetGlobalOption("clipboard").(string))
		clipErr := clipboard.Initialize(m)

		defer func() {
			if err := recover(); err != nil {
				if screen.Screen != nil {
					screen.Screen.Fini()
				}
				if e, ok := err.(*lua.ApiError); ok {
					fmt.Println("Lua API error:", e)
				} else {
					fmt.Println("Micro encountered an error:", errors.Wrap(err, 2).ErrorStack(), "\nIf you can reproduce this error, please report it at https://github.com/zyedidia/micro/issues")
				}
				// backup all open buffers
				for _, b := range buffer.OpenBuffers {
					_ = b.Backup()
				}
				os.Exit(1)
			}
		}()

		err = config.LoadAllPlugins()
		if err != nil {
			screen.TermMessage(err)
		}

		action.InitBindings()
		action.InitCommands()

		err = config.InitColorscheme()
		if err != nil {
			screen.TermMessage(err)
		}

		err = config.RunPluginFn("preinit")
		if err != nil {
			screen.TermMessage(err)
		}

		action.InitGlobals()
		buffer.SetMessager(action.InfoBar)
		args := ctx.Args().Slice()
		b := LoadInput(args)

		if len(b) == 0 {
			// No buffers to open
			screen.Screen.Fini()
			runtime.Goexit()
		}

		action.InitTabs(b)

		err = config.RunPluginFn("init")
		if err != nil {
			screen.TermMessage(err)
		}

		err = config.RunPluginFn("postinit")
		if err != nil {
			screen.TermMessage(err)
		}

		if clipErr != nil {
			log.Println(clipErr, " or change 'clipboard' option")
		}

		if a := config.GetGlobalOption("autosave").(float64); a > 0 {
			config.SetAutoTime(int(a))
			config.StartAutoSave()
		}

		screen.Events = make(chan tcell.Event)

		sigterm = make(chan os.Signal, 1)
		sighup = make(chan os.Signal, 1)
		signal.Notify(sigterm, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGABRT)
		signal.Notify(sighup, syscall.SIGHUP)

		// Here is the event loop which runs in a separate thread
		go func() {
			for {
				screen.Lock()
				e := screen.Screen.PollEvent()
				screen.Unlock()
				if e != nil {
					screen.Events <- e
				}
			}
		}()

		// clear the drawchan so we don't redraw excessively
		// if someone requested a redraw before we started displaying
		for len(screen.DrawChan()) > 0 {
			<-screen.DrawChan()
		}

		// wait for initial resize event
		select {
		case event := <-screen.Events:
			action.Tabs.HandleEvent(event)
		case <-time.After(10 * time.Millisecond):
			// time out after 10ms
		}

		for {
			DoEvent()
		}
	},
}

func init() {
	optionsFlags = make(map[string]cli.Flag)
	for k, v := range config.DefaultAllSettings() {
		optionsFlags[k] = &cli.StringFlag{
			Name:  k,
			Usage: fmt.Sprintf("The %s option", k),
			Value: fmt.Sprintf("%v", v),
		}
	}
	for _, v := range optionsFlags {
		app.Flags = append(app.Flags, v)
	}
}

// LoadInput determines which files should be loaded into buffers
// based on the input stored in flag.Args()
func LoadInput(args []string) []*buffer.Buffer {
	// There are a number of ways micro should start given its input

	// 1. If it is given a files in flag.Args(), it should open those

	// 2. If there is no input file and the input is not a terminal, that means
	// something is being piped in and the stdin should be opened in an
	// empty buffer

	// 3. If there is no input file and the input is a terminal, an empty buffer
	// should be opened

	var filename string
	var input []byte
	var err error
	buffers := make([]*buffer.Buffer, 0, len(args))

	btype := buffer.BTDefault
	if !isatty.IsTerminal(os.Stdout.Fd()) {
		btype = buffer.BTStdout
	}

	files := make([]string, 0, len(args))
	flagStartPos := buffer.Loc{X: -1, Y: -1}
	flagr := regexp.MustCompile(`^\+(\d+)(?::(\d+))?$`)
	for _, a := range args {
		match := flagr.FindStringSubmatch(a)
		if len(match) == 3 && match[2] != "" {
			line, err := strconv.Atoi(match[1])
			if err != nil {
				screen.TermMessage(err)
				continue
			}
			col, err := strconv.Atoi(match[2])
			if err != nil {
				screen.TermMessage(err)
				continue
			}
			flagStartPos = buffer.Loc{X: col - 1, Y: line - 1}
		} else if len(match) == 3 && match[2] == "" {
			line, err := strconv.Atoi(match[1])
			if err != nil {
				screen.TermMessage(err)
				continue
			}
			flagStartPos = buffer.Loc{Y: line - 1}
		} else {
			files = append(files, a)
		}
	}

	if len(files) > 0 {
		// Option 1
		// We go through each file and load it
		for i := 0; i < len(files); i++ {
			buf, err := buffer.NewBufferFromFileAtLoc(files[i], btype, flagStartPos)
			if err != nil {
				screen.TermMessage(err)
				continue
			}
			// If the file didn't exist, input will be empty, and we'll open an empty buffer
			buffers = append(buffers, buf)
		}
	} else if !isatty.IsTerminal(os.Stdin.Fd()) {
		// Option 2
		// The input is not a terminal, so something is being piped in
		// and we should read from stdin
		input, err = io.ReadAll(os.Stdin)
		if err != nil {
			screen.TermMessage("Error reading from stdin: ", err)
			input = []byte{}
		}
		buffers = append(buffers, buffer.NewBufferFromStringAtLoc(string(input), filename, btype, flagStartPos))
	} else {
		// Option 3, just open an empty buffer
		buffers = append(buffers, buffer.NewBufferFromStringAtLoc(string(input), filename, btype, flagStartPos))
	}

	return buffers
}

func main() {
	defer func() {
		if util.Stdout.Len() > 0 {
			_, _ = fmt.Fprint(os.Stdout, util.Stdout.String())
		}
		os.Exit(0)
	}()

	var err error

	if err = app.Run(os.Args); err != nil {
		_, _ = fmt.Fprintln(app.ErrWriter, err)
		os.Exit(1)
	}
}

// DoEvent runs the main action loop of the editor
func DoEvent() {
	var event tcell.Event

	// Display everything
	screen.Screen.Fill(' ', config.DefStyle)
	screen.Screen.HideCursor()
	action.Tabs.Display()
	for _, ep := range action.MainTab().Panes {
		ep.Display()
	}
	action.MainTab().Display()
	action.InfoBar.Display()
	screen.Screen.Show()

	// Check for new events
	select {
	case f := <-shell.Jobs:
		// If a new job has finished while running in the background we should execute the callback
		ulua.Lock.Lock()
		f.Function(f.Output, f.Args)
		ulua.Lock.Unlock()
	case <-config.Autosave:
		ulua.Lock.Lock()
		for _, b := range buffer.OpenBuffers {
			_ = b.Save()
		}
		ulua.Lock.Unlock()
	case <-shell.CloseTerms:
	case event = <-screen.Events:
	case <-screen.DrawChan():
		for len(screen.DrawChan()) > 0 {
			<-screen.DrawChan()
		}
	case <-sighup:
		for _, b := range buffer.OpenBuffers {
			if !b.Modified() {
				b.Fini()
			}
		}
		os.Exit(0)
	case <-sigterm:
		for _, b := range buffer.OpenBuffers {
			if !b.Modified() {
				b.Fini()
			}
		}

		if screen.Screen != nil {
			screen.Screen.Fini()
		}
		os.Exit(0)
	}

	if e, ok := event.(*tcell.EventError); ok {
		log.Println("tcell event error: ", e.Error())

		if e.Err() == io.EOF {
			// shutdown due to terminal closing/becoming inaccessible
			for _, b := range buffer.OpenBuffers {
				if !b.Modified() {
					b.Fini()
				}
			}

			if screen.Screen != nil {
				screen.Screen.Fini()
			}
			os.Exit(0)
		}
		return
	}

	ulua.Lock.Lock()
	// if event != nil {
	if action.InfoBar.HasPrompt {
		action.InfoBar.HandleEvent(event)
	} else {
		action.Tabs.HandleEvent(event)
	}
	// }
	ulua.Lock.Unlock()
}
