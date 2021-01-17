package main

import (
	"context"
	stdlog "log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tarosky/gutenberg-notifier/notify"
	"github.com/urfave/cli/v2"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func createLogger() *zap.Logger {
	cfg := zap.NewDevelopmentConfig()
	cfg.EncoderConfig.EncodeTime = zapcore.TimeEncoderOfLayout("2006-01-02T15:04:05.000000Z0700")
	log, err := cfg.Build(zap.WithCaller(false))
	if err != nil {
		panic("failed to initialize logger")
	}

	return log
}

var (
	log *zap.Logger
)

func main() {
	app := cli.NewApp()
	app.Name = "nudge"
	app.Description = "Notify file changes"

	app.Flags = []cli.Flag{
		&cli.StringSliceFlag{
			Name:    "excl-comm",
			Aliases: []string{"ec"},
			Value:   &cli.StringSlice{},
			Usage:   "Command name to be excluded",
		},
		&cli.StringSliceFlag{
			Name:    "incl-fmode",
			Aliases: []string{"im"},
			Value:   &cli.StringSlice{},
			Usage:   "File operation mode to be included. Possible values are: " + strings.Join(notify.AllFModes(), ", ") + ".",
		},
		&cli.StringSliceFlag{
			Name:    "incl-fullname",
			Aliases: []string{"in"},
			Value:   &cli.StringSlice{},
			Usage:   "Full file name to be included.",
		},
		&cli.StringSliceFlag{
			Name:    "incl-ext",
			Aliases: []string{"ie"},
			Value:   &cli.StringSlice{},
			Usage:   "File with specified extension to be included. Include leading dot.",
		},
		&cli.StringSliceFlag{
			Name:    "incl-mntpath",
			Aliases: []string{"ir"},
			Value:   &cli.StringSlice{},
			Usage:   "Full path to the mount point where the file is located. Never include trailing slash.",
		},
		&cli.IntFlag{
			Name:    "max-mnt-depth",
			Aliases: []string{"md"},
			Value:   16,
			Usage:   "Maximum depth to scan for getting absolute mount point path. Increasing this value too much could cause compilation failure.",
		},
		&cli.IntFlag{
			Name:    "max-dir-depth",
			Aliases: []string{"dd"},
			Value:   32,
			Usage:   "Maximum depth to scan for getting absolute file path. Increasing this value too much could cause compilation failure.",
		},
		&cli.PathFlag{
			Name:     "nudge-file",
			Aliases:  []string{"f"},
			Required: true,
			Usage:    "File to nudge when change happens.",
		},
		&cli.StringFlag{
			Name:    "nudge-type",
			Aliases: []string{"t"},
			Value:   "mtime",
			Usage:   "The way gutenberg-nudge nudges using the file. Currently the only supported value is mtime.",
		},
		&cli.IntFlag{
			Name:    "nudge-interval",
			Aliases: []string{"i"},
			Value:   1000,
			Usage:   "Minimum interval between nudges.",
		},
	}

	app.Action = func(c *cli.Context) error {
		log = createLogger()
		defer log.Sync()

		cfg := &notify.Config{
			ExclComms:     c.StringSlice("excl-comm"),
			InclFullNames: c.StringSlice("incl-fullname"),
			InclExts:      c.StringSlice("incl-ext"),
			InclMntPaths:  c.StringSlice("incl-mntpath"),
			MaxMntDepth:   c.Int("max-mnt-depth"),
			MaxDirDepth:   c.Int("max-dir-depth"),
			BpfDebug:      0,
			Quit:          false,
			Log:           log,
		}

		if err := cfg.SetModesFromString(c.StringSlice("incl-fmode")); err != nil {
			log.Panic("illegal incl-fmode parameter", zap.Error(err))
		}

		if nudgeType := c.String("nudge-type"); nudgeType != "mtime" {
			log.Panic("illegal nudge-type parameter", zap.String("parameter", nudgeType))
		}

		ng := newNudger(c.Path("nudge-file"))
		nudgeInterval := c.Int("nudge-interval")

		eventCh := make(chan *notify.Event)
		ctx, cancel := context.WithCancel(context.Background())

		sig := make(chan os.Signal)
		signal.Notify(sig, os.Interrupt, os.Kill, syscall.SIGTERM, syscall.SIGQUIT)
		go func() {
			defer func() {
				signal.Stop(sig)
				close(sig)
			}()

			<-sig
			cancel()
		}()

		listenAndNudge(ng, nudgeInterval, eventCh)
		notify.Run(ctx, cfg, eventCh)

		return nil
	}

	err := app.Run(os.Args)
	if err != nil {
		stdlog.Panic("failed to run app", zap.Error(err))
	}
}

func listenAndNudge(nudger *nudger, interval int, eventCh <-chan *notify.Event) {
	go func() {
		pace := time.Duration(interval) * time.Millisecond
		pacemaker := time.NewTicker(pace)
		pacemakerHasBaton := false
		for {
			select {
			case t := <-pacemaker.C:
				if pacemakerHasBaton {
					if err := nudger.nudge(&t); err != nil {
						log.Error("failed to nudge", zap.Error(err))
					} else {
						log.Debug("nudged", zap.Time("mtime", t))
					}
					pacemakerHasBaton = false
					continue
				}

				// Do nothing
			case _, ok := <-eventCh:
				if !ok {
					pacemaker.Stop()
					return
				}

				if pacemakerHasBaton {
					continue
				}

				t := time.Now()
				if nudger.last == nil {
					nudger.nudge(&t)
					log.Debug("nudged", zap.Time("mtime", t))
					pacemaker.Reset(pace)
					continue
				}

				if (*nudger.last).Add(pace).Before(t) {
					nudger.nudge(&t)
					log.Debug("nudged", zap.Time("mtime", t))
					pacemaker.Reset(pace)
					continue
				}

				pacemakerHasBaton = true
			}
		}
	}()
}

type nudger struct {
	path  string
	first bool
	last  *time.Time
}

func newNudger(nudgeFile string) *nudger {
	if _, err := os.Stat(nudgeFile); err != nil {
		f, err := os.Create(nudgeFile)
		if err != nil {
			log.Panic("unable to create nudge file", zap.Error(err))
		}
		log.Info("no nudge file found. created.", zap.String("path", nudgeFile))
		f.Close()
	}

	return &nudger{path: nudgeFile, first: true}
}

func (n *nudger) nudge(t *time.Time) error {
	if n.first {
		n.first = false
		if _, err := os.Stat(n.path); err != nil {
			f, err := os.Create(n.path)
			if err != nil {
				return err
			}
			f.Close()
			n.last = t
			return nil
		}
	}

	if err := os.Chtimes(n.path, *t, *t); err != nil {
		return err
	}

	n.last = t
	return nil
}
