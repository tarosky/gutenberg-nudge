package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	stdlog "log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tarosky/gutenberg-notifier/notify"
	"github.com/urfave/cli/v2"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sync/errgroup"
)

var (
	log *zap.Logger
)

// This implements zapcore.WriteSyncer interface.
type lockedFileWriteSyncer struct {
	m    sync.Mutex
	f    *os.File
	path string
}

func newLockedFileWriteSyncer(path string) *lockedFileWriteSyncer {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0666)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error while creating log file: path: %s", err.Error())
		panic(err)
	}

	return &lockedFileWriteSyncer{
		f:    f,
		path: path,
	}
}

func (s *lockedFileWriteSyncer) Write(bs []byte) (int, error) {
	s.m.Lock()
	defer s.m.Unlock()

	return s.f.Write(bs)
}

func (s *lockedFileWriteSyncer) Sync() error {
	s.m.Lock()
	defer s.m.Unlock()

	return s.f.Sync()
}

func (s *lockedFileWriteSyncer) reopen() {
	s.m.Lock()
	defer s.m.Unlock()

	if err := s.f.Close(); err != nil {
		fmt.Fprintf(
			os.Stderr, "error while reopening file: path: %s, err: %s", s.path, err.Error())
	}

	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0666)
	if err != nil {
		fmt.Fprintf(
			os.Stderr, "error while reopening file: path: %s, err: %s", s.path, err.Error())
		panic(err)
	}

	s.f = f
}

func (s *lockedFileWriteSyncer) Close() error {
	s.m.Lock()
	defer s.m.Unlock()

	return s.f.Close()
}

func createLogger(ctx context.Context, logPath, errorLogPath string) *zap.Logger {
	enc := zapcore.NewJSONEncoder(zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "level",
		NameKey:        zapcore.OmitKey,
		CallerKey:      zapcore.OmitKey,
		FunctionKey:    zapcore.OmitKey,
		MessageKey:     "message",
		StacktraceKey:  zapcore.OmitKey,
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.CapitalLevelEncoder,
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeDuration: zapcore.StringDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	})

	out := newLockedFileWriteSyncer(logPath)
	errOut := newLockedFileWriteSyncer(errorLogPath)

	sigusr1 := make(chan os.Signal, 1)
	signal.Notify(sigusr1, syscall.SIGUSR1)

	go func() {
		for {
			select {
			case _, ok := <-sigusr1:
				if !ok {
					break
				}
				out.reopen()
				errOut.reopen()
			case <-ctx.Done():
				signal.Stop(sigusr1)
				// closing sigusr1 causes panic (close of closed channel)
				break
			}
		}
	}()

	return zap.New(
		zapcore.NewCore(enc, out, zap.NewAtomicLevelAt(zap.DebugLevel)),
		zap.ErrorOutput(errOut),
		zap.Development(),
		zap.WithCaller(false))
}

func setPIDFile(path string) func() {
	if path == "" {
		return func() {}
	}

	pid := []byte(strconv.Itoa(os.Getpid()))
	if err := ioutil.WriteFile(path, pid, 0644); err != nil {
		log.Panic(
			"failed to create PID file",
			zap.String("path", path),
			zap.Error(err))
	}

	return func() {
		if err := os.Remove(path); err != nil {
			log.Error(
				"failed to remove PID file",
				zap.String("path", path),
				zap.Error(err))
		}
	}
}

type FileWatch struct {
	IncludedFullNames  []string `json:"included-full-names"`
	IncludedExtensions []string `json:"included-extensions"`
	IncludedMountPaths []string `json:"included-mount-paths"`
	MaxDirDepth        int      `json:"max-directory-depth"`
	MaxMountDepth      int      `json:"max-mount-depth"`
	NotificationFile   string   `json:"notification-file"`
}

type WatchedDir struct {
	PathFromMountPoint           string `json:"path-from-mount-point"`
	NotificationFile             string `json:"notification-file"`
	NotificationWithAdditionFile string `json:"notification-with-addition-file"`
}

func (d *WatchedDir) normalizedPathFromMountPoint() string {
	return strings.Trim(d.PathFromMountPoint, "/") + "/"
}

type DirsWatch struct {
	IncludedDirs                     []WatchedDir `json:"included-directories"`
	MountPath                        string       `json:"mount-path"`
	AdditionalNotificationExtensions []string     `json:"additional-notification-extensions"`
	MaxDirDepth                      int          `json:"max-directory-depth"`
	MaxMountDepth                    int          `json:"max-mount-depth"`
}

func (w *DirsWatch) includedDirPaths() []string {
	ds := make([]string, 0, len(w.IncludedDirs))
	for _, d := range w.IncludedDirs {
		ds = append(ds, d.normalizedPathFromMountPoint())
	}

	return ds
}

type Watches struct {
	File FileWatch `json:"file"`
	Dirs DirsWatch `json:"directories"`
}

type config struct {
	Watches Watches `json:"watches"`
}

func loadConfig(log *zap.Logger, path string) *config {
	file, err := os.Open(path)
	if err != nil {
		log.Panic("failed to open config file", zap.Error(err))
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Error("failed to close config file", zap.Error(err))
		}
	}()

	data, err := ioutil.ReadAll(file)
	if err != nil {
		log.Error("failed to read config file", zap.Error(err))
	}

	cfg := &config{}
	if err := json.Unmarshal(data, cfg); err != nil {
		log.Panic("Illegal config file", zap.Error(err))
	}

	if cfg.Watches.File.IncludedFullNames == nil {
		cfg.Watches.File.IncludedFullNames = []string{}
	}

	if cfg.Watches.File.IncludedExtensions == nil {
		cfg.Watches.File.IncludedExtensions = []string{}
	}

	if cfg.Watches.File.IncludedMountPaths == nil {
		cfg.Watches.File.IncludedMountPaths = []string{}
	}

	if cfg.Watches.Dirs.IncludedDirs == nil {
		cfg.Watches.Dirs.IncludedDirs = []WatchedDir{}
	}

	if cfg.Watches.Dirs.AdditionalNotificationExtensions == nil {
		cfg.Watches.Dirs.AdditionalNotificationExtensions = []string{}
	}

	return cfg
}

func dirsWithTrailingSlash(dirs []string) []string {
	ds := make([]string, 0, len(dirs))
	for _, d := range dirs {
		ds = append(ds, strings.TrimRight(d, "/")+"/")
	}
	return ds
}

type pathNudgers struct {
	nudger   *nudgerWithAddition
	interval int
	eventCh  chan *notify.Event
}

type dirsNudgers map[string]*pathNudgers

func newDirsNudgersFromConfig(c *config, interval int) dirsNudgers {
	ns := dirsNudgers{}
	for _, d := range c.Watches.Dirs.IncludedDirs {
		ns[d.normalizedPathFromMountPoint()] = &pathNudgers{
			nudger:   newNudgerWithAddition(d.NotificationFile, d.NotificationWithAdditionFile),
			interval: interval,
			eventCh:  make(chan *notify.Event),
		}
	}
	return ns
}

func (n *pathNudgers) pathListenAndNudge(additionalNotifExts []string) {
	go func() {
		pace := time.Duration(n.interval) * time.Millisecond
		pacemaker := time.NewTicker(pace)
		pacemakerHasBaton := false
		addition := false

		for {
			select {
			case t := <-pacemaker.C:
				if pacemakerHasBaton {
					if err := n.nudger.nudge(&t, addition); err != nil {
						log.Error("failed to nudge", zap.Error(err))
					} else {
						log.Debug("nudged", zap.Time("mtime", t), zap.Bool("addition", addition))
					}
					pacemakerHasBaton = false
					addition = false
					continue
				}

				// Do nothing
			case ev, ok := <-n.eventCh:
				if !ok {
					return
				}

				for _, ext := range additionalNotifExts {
					if filepath.Ext(ev.Name) == ext {
						addition = true
					}
				}
				if ev.Name == "" {
					addition = true
				}

				if pacemakerHasBaton {
					continue
				}

				t := time.Now()
				if n.nudger.last == nil || (*n.nudger.last).Add(pace).Before(t) {
					n.nudger.nudge(&t, addition)
					log.Debug("nudged", zap.Time("mtime", t))
					pacemaker.Reset(pace)
					addition = false
					continue
				}

				pacemakerHasBaton = true
			}
		}
	}()
}

func main() {
	app := cli.NewApp()
	app.Name = "nudge"
	app.Description = "Notify file changes"

	app.Flags = []cli.Flag{
		&cli.PathFlag{
			Name:    "config-file",
			Aliases: []string{"c"},
			Usage:   "Path to configuration file.",
		},
		&cli.IntFlag{
			Name:    "nudge-interval",
			Aliases: []string{"i"},
			Value:   1000,
			Usage:   "Minimum interval between nudges.",
		},
		&cli.PathFlag{
			Name:     "log-path",
			Aliases:  []string{"l"},
			Required: true,
		},
		&cli.PathFlag{
			Name:     "error-log-path",
			Aliases:  []string{"el"},
			Required: true,
		},
		&cli.PathFlag{
			Name:    "pid-file",
			Aliases: []string{"id"},
		},
	}

	app.Action = func(c *cli.Context) error {
		mustGetAbsPath := func(name string) string {
			path, err := filepath.Abs(c.Path(name))
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to get %s: %s", name, err.Error())
				panic(err)
			}
			return path
		}

		log = createLogger(
			c.Context,
			mustGetAbsPath("log-path"),
			mustGetAbsPath("error-log-path"))
		defer log.Sync()

		cfg := loadConfig(log, mustGetAbsPath("config-file"))

		fileNotifConfig := &notify.Config{
			ExclComms:        []string{},
			InclFModes:       notify.FModeWrite,
			InclPathPrefixes: []string{},
			InclFullNames:    cfg.Watches.File.IncludedFullNames,
			InclExts:         cfg.Watches.File.IncludedExtensions,
			InclMntPaths:     cfg.Watches.File.IncludedMountPaths,
			MaxMntDepth:      cfg.Watches.File.MaxMountDepth,
			MaxDirDepth:      cfg.Watches.File.MaxDirDepth,
			BpfDebug:         0,
			Quit:             false,
			Log:              log,
		}

		dirNotifConfig := &notify.Config{
			ExclComms:        []string{},
			InclFModes:       notify.FModeWrite,
			InclPathPrefixes: cfg.Watches.Dirs.includedDirPaths(),
			InclFullNames:    []string{},
			InclExts:         []string{},
			InclMntPaths:     []string{cfg.Watches.Dirs.MountPath},
			MaxMntDepth:      cfg.Watches.Dirs.MaxMountDepth,
			MaxDirDepth:      cfg.Watches.Dirs.MaxDirDepth,
			BpfDebug:         0,
			Quit:             false,
			Log:              log,
		}

		nudgeInterval := c.Int("nudge-interval")

		removePIDFile := setPIDFile(mustGetAbsPath("pid-file"))
		defer removePIDFile()

		fileEventCh := make(chan *notify.Event)
		dirsEventCh := make(chan *notify.Event)
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

		fileNudger := newNudger(cfg.Watches.File.NotificationFile)
		fileListenAndNudge(fileNudger, nudgeInterval, fileEventCh)

		dirsNudgers := newDirsNudgersFromConfig(cfg, nudgeInterval)
		dirsListenAndNudge(
			dirsNudgers,
			cfg.Watches.Dirs.AdditionalNotificationExtensions,
			nudgeInterval,
			dirsEventCh)

		eg := &errgroup.Group{}

		eg.Go(func() error {
			notify.Run(ctx, fileNotifConfig, fileEventCh)
			return nil
		})

		eg.Go(func() error {
			notify.Run(ctx, dirNotifConfig, dirsEventCh)
			return nil
		})

		if err := eg.Wait(); err != nil {
			panic("should not happen")
		}

		return nil
	}

	err := app.Run(os.Args)
	if err != nil {
		stdlog.Panic("failed to run app", zap.Error(err))
	}
}

func fileListenAndNudge(nudger *nudger, interval int, fileEventCh <-chan *notify.Event) {
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
			case _, ok := <-fileEventCh:
				if !ok {
					return
				}

				if pacemakerHasBaton {
					continue
				}

				t := time.Now()
				if nudger.last == nil || (*nudger.last).Add(pace).Before(t) {
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

func dirsListenAndNudge(
	nudgers dirsNudgers,
	additionalNotifExts []string,
	interval int,
	dirsEventCh <-chan *notify.Event,
) {
	for _, n := range nudgers {
		n.pathListenAndNudge(additionalNotifExts)
	}

	go func() {
		for ev := range dirsEventCh {
			if ev.PathFromMount == "" {
				for _, n := range nudgers {
					n.eventCh <- ev
				}
				continue
			}

			for p, n := range nudgers {
				if strings.HasPrefix(ev.PathFromMount, p) {
					n.eventCh <- ev
				}
			}
		}

		for _, n := range nudgers {
			close(n.eventCh)
		}
	}()
}

type nudger struct {
	path string
	last *time.Time
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

	return &nudger{path: nudgeFile}
}

func (n *nudger) nudge(t *time.Time) error {
	if err := os.Chtimes(n.path, *t, *t); err != nil {
		return err
	}

	n.last = t
	return nil
}

type nudgerWithAddition struct {
	path             string
	pathWithAddition string
	last             *time.Time
}

func newNudgerWithAddition(nudgeFile, nudgerFileWithAddition string) *nudgerWithAddition {
	if _, err := os.Stat(nudgeFile); err != nil {
		f, err := os.Create(nudgeFile)
		if err != nil {
			log.Panic("unable to create nudge file", zap.Error(err))
		}
		log.Info("no nudge file found. created.", zap.String("path", nudgeFile))
		f.Close()
	}

	if _, err := os.Stat(nudgerFileWithAddition); err != nil {
		f, err := os.Create(nudgerFileWithAddition)
		if err != nil {
			log.Panic("unable to create nudgeWithAddition file", zap.Error(err))
		}
		log.Info("no nudgeWithAddition file found. created.", zap.String("path", nudgerFileWithAddition))
		f.Close()
	}

	return &nudgerWithAddition{
		path:             nudgeFile,
		pathWithAddition: nudgerFileWithAddition,
	}
}

func (n *nudgerWithAddition) nudge(t *time.Time, addition bool) error {
	var path string
	if addition {
		path = n.pathWithAddition
	} else {
		path = n.path
	}

	if err := os.Chtimes(path, *t, *t); err != nil {
		return err
	}

	n.last = t
	return nil
}
