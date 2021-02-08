package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
	"github.com/gorilla/mux"
	"github.com/karimra/gnmic/collector"
	"github.com/karimra/gnmic/config"
	"github.com/karimra/gnmic/formatters"
	"github.com/openconfig/gnmi/proto/gnmi"
	"github.com/openconfig/goyang/pkg/yang"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/protobuf/proto"
)

type App struct {
	version string
	commit  string
	date    string
	gitURL  string

	ctx     context.Context
	Cfn     context.CancelFunc
	RootCmd *cobra.Command

	m             *sync.Mutex
	Config        *config.Config
	collector     *collector.Collector
	router        *mux.Router
	Logger        *log.Logger
	out           io.Writer
	PromptMode    bool
	PromptHistory []string
	SchemaTree    *yang.Entry

	wg        *sync.WaitGroup
	printLock *sync.Mutex
}

func New() *App {
	ctx, cancel := context.WithCancel(context.Background())
	return &App{
		ctx:           ctx,
		Cfn:           cancel,
		RootCmd:       new(cobra.Command),
		m:             new(sync.Mutex),
		Config:        config.New(),
		router:        mux.NewRouter(),
		Logger:        log.New(ioutil.Discard, "", log.LstdFlags),
		out:           os.Stdout,
		PromptHistory: make([]string, 0, 128),
		SchemaTree: &yang.Entry{
			Dir: make(map[string]*yang.Entry),
		},

		wg:        new(sync.WaitGroup),
		printLock: new(sync.Mutex)}
}

func (a *App) PreRun(_ *cobra.Command, args []string) error {
	a.Config.SetLogger()
	a.Config.SetPersistantFlagsFromFile(a.RootCmd)
	a.Config.Globals.Address = config.SanitizeArrayFlagValue(a.Config.Globals.Address)
	a.Logger = log.New(ioutil.Discard, "[gnmic] ", log.LstdFlags|log.Lmicroseconds)
	if a.Config.Globals.LogFile != "" {
		f, err := os.OpenFile(a.Config.Globals.LogFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		if err != nil {
			return fmt.Errorf("error opening log file: %v", err)
		}
		a.Logger.SetOutput(f)
	} else {
		if a.Config.Globals.Debug {
			a.Config.Globals.Log = true
		}
		if a.Config.Globals.Log {
			a.Logger.SetOutput(os.Stderr)
		}
	}
	if a.Config.Globals.Debug {
		a.Logger.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Llongfile)
	}

	if a.Config.Globals.Debug {
		grpclog.SetLogger(a.Logger) //lint:ignore SA1019 see https://github.com/karimra/gnmic/issues/59
		a.Logger.Printf("version=%s, commit=%s, date=%s, gitURL=%s, docs=https://gnmic.kmrd.dev", a.version, a.commit, a.date, a.gitURL)
	}
	cfgFile := a.Config.FileConfig.ConfigFileUsed()
	if len(cfgFile) != 0 {
		a.Logger.Printf("using config file %s", cfgFile)
		b, err := ioutil.ReadFile(cfgFile)
		if err != nil {
			if a.RootCmd.Flag("config").Changed {
				return err
			}
			a.Logger.Printf("failed reading config file: %v", err)
		}
		if a.Config.Globals.Debug {
			a.Logger.Printf("config file:\n%s", string(b))
		}
	}
	// logConfig
	return nil
}

func (a *App) Print(address string, msgName string, msg proto.Message) error {
	a.printLock.Lock()
	defer a.printLock.Unlock()
	fmt.Fprint(os.Stderr, msgName)
	fmt.Fprintln(os.Stderr, "")
	printPrefix := ""
	if len(a.Config.TargetsList()) > 1 && !a.Config.Globals.NoPrefix {
		printPrefix = fmt.Sprintf("[%s] ", address)
	}

	switch msg := msg.ProtoReflect().Interface().(type) {
	case *gnmi.CapabilityResponse:
		if len(a.Config.Globals.Format) == 0 {
			a.printCapResponse(printPrefix, msg)
			return nil
		}
	}
	mo := formatters.MarshalOptions{
		Multiline: true,
		Indent:    "  ",
		Format:    a.Config.Globals.Format,
	}
	b, err := mo.Marshal(msg, map[string]string{"address": address})
	if err != nil {
		a.Logger.Printf("error marshaling capabilities request: %v", err)
		if !a.Config.Globals.Log {
			fmt.Printf("error marshaling capabilities request: %v", err)
		}
		return err
	}
	sb := strings.Builder{}
	sb.Write(b)
	fmt.Fprintf(a.out, "%s\n", indent(printPrefix, sb.String()))
	return nil
}

func (a *App) createCollectorDialOpts() []grpc.DialOption {
	opts := []grpc.DialOption{}
	opts = append(opts, grpc.WithBlock())
	if a.Config.Globals.MaxMsgSize > 0 {
		opts = append(opts, grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(a.Config.Globals.MaxMsgSize)))
	}
	if !a.Config.Globals.ProxyFromEnv {
		opts = append(opts, grpc.WithNoProxy())
	}
	return opts
}

func (a *App) watchConfig() {
	a.Logger.Printf("watching config...")
	a.Config.FileConfig.OnConfigChange(a.loadTargets)
	a.Config.FileConfig.WatchConfig()
}

func (a *App) loadTargets(e fsnotify.Event) {
	a.Logger.Printf("got config change notification: %v", e)
	a.m.Lock()
	defer a.m.Unlock()
	switch e.Op {
	case fsnotify.Write, fsnotify.Create:
		newTargets, err := a.Config.GetTargets()
		if err != nil && !errors.Is(err, config.ErrNoTargetsFound) {
			a.Logger.Printf("failed getting targets from new config: %v", err)
			return
		}
		currentTargets := a.collector.Targets
		// delete targets
		for n := range currentTargets {
			if _, ok := newTargets[n]; !ok {
				if a.Config.Globals.Debug {
					a.Logger.Printf("target %q deleted from config", n)
				}
				err = a.collector.DeleteTarget(n)
				if err != nil {
					a.Logger.Printf("failed to delete target %q: %v", n, err)
				}
			}
		}
		// add targets
		for n, tc := range newTargets {
			if _, ok := currentTargets[n]; !ok {
				if a.Config.Globals.Debug {
					a.Logger.Printf("target %q added to config", n)
				}
				err = a.collector.AddTarget(tc)
				if err != nil {
					a.Logger.Printf("failed adding target %q: %v", n, err)
					continue
				}
				a.wg.Add(1)
				go a.collector.InitTarget(a.ctx, n)
			}
		}
	}
}

func (a *App) startAPI() {
	if a.Config.Globals.API != "" {
		a.routes()
		s := &http.Server{
			Addr:    a.Config.Globals.API,
			Handler: a.router,
		}
		err := s.ListenAndServe()
		if err != nil {
			a.Logger.Printf("API server err: %v", err)
			return
		}
	}
}
