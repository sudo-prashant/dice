package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dicedb/dice/internal/logger"
	"github.com/dicedb/dice/internal/server/abstractserver"
	"github.com/dicedb/dice/internal/wal"
	"github.com/dicedb/dice/internal/watchmanager"

	"github.com/dicedb/dice/config"
	diceerrors "github.com/dicedb/dice/internal/errors"
	"github.com/dicedb/dice/internal/observability"
	"github.com/dicedb/dice/internal/server"
	"github.com/dicedb/dice/internal/server/resp"
	"github.com/dicedb/dice/internal/shard"
	dstore "github.com/dicedb/dice/internal/store"
	"github.com/dicedb/dice/internal/worker"
)

type configEntry struct {
	Key   string
	Value interface{}
}

var configTable = []configEntry{}

func init() {
	flag.StringVar(&config.Host, "host", "0.0.0.0", "host for the DiceDB server")

	flag.IntVar(&config.Port, "port", 7379, "port for the DiceDB server")

	flag.IntVar(&config.HTTPPort, "http-port", 7380, "port for accepting requets over HTTP")
	flag.BoolVar(&config.EnableHTTP, "enable-http", false, "enable DiceDB to listen, accept, and process HTTP")

	flag.IntVar(&config.WebsocketPort, "websocket-port", 7381, "port for accepting requets over WebSocket")
	flag.BoolVar(&config.EnableWebsocket, "enable-websocket", false, "enable DiceDB to listen, accept, and process WebSocket")

	flag.BoolVar(&config.EnableMultiThreading, "enable-multithreading", false, "enable multithreading execution and leverage multiple CPU cores")
	flag.IntVar(&config.NumShards, "num-shards", -1, "number shards to create. defaults to number of cores")

	flag.BoolVar(&config.EnableWatch, "enable-watch", false, "enable support for .WATCH commands and real-time reactivity")
	flag.BoolVar(&config.EnableProfiling, "enable-profiling", false, "enable profiling and capture critical metrics and traces in .prof files")

	flag.StringVar(&config.DiceConfig.Logging.LogLevel, "log-level", "info", "log level, values: info, debug")
	flag.StringVar(&config.LogDir, "log-dir", "/tmp/dicedb", "log directory path")

	flag.BoolVar(&config.EnableWAL, "enable-wal", false, "enable write-ahead logging")
	flag.BoolVar(&config.RestoreFromWAL, "restore-wal", false, "restore the database from the WAL files")
	flag.StringVar(&config.WALEngine, "wal-engine", "null", "wal engine to use, values: sqlite, aof")

	flag.StringVar(&config.RequirePass, "requirepass", config.RequirePass, "enable authentication for the default user")
	flag.StringVar(&config.CustomConfigFilePath, "o", config.CustomConfigFilePath, "dir path to create the config file")
	flag.StringVar(&config.FileLocation, "c", config.FileLocation, "file path of the config file")
	flag.BoolVar(&config.InitConfigCmd, "init-config", false, "initialize a new config file")
	flag.IntVar(&config.KeysLimit, "keys-limit", config.KeysLimit, "keys limit for the DiceDB server. "+
		"This flag controls the number of keys each shard holds at startup. You can multiply this number with the "+
		"total number of shard threads to estimate how much memory will be required at system start up.")

	flag.Parse()

	config.SetupConfig()

	iid := observability.GetOrCreateInstanceID()
	config.DiceConfig.InstanceID = iid

	slog.SetDefault(logger.New())
}

func printSplash() {
	fmt.Print(`
	██████╗ ██╗ ██████╗███████╗██████╗ ██████╗ 
	██╔══██╗██║██╔════╝██╔════╝██╔══██╗██╔══██╗
	██║  ██║██║██║     █████╗  ██║  ██║██████╔╝
	██║  ██║██║██║     ██╔══╝  ██║  ██║██╔══██╗
	██████╔╝██║╚██████╗███████╗██████╔╝██████╔╝
	╚═════╝ ╚═╝ ╚═════╝╚══════╝╚═════╝ ╚═════╝
			
	`)
}

// configuration function used to add configuration values to the print table at the startup.
// add entry to this function to add a new row in the startup configuration table.
func configuration() {
	// Add the version of the DiceDB to the configuration table
	addEntry("Version", config.DiceDBVersion)

	// Add the port number on which DiceDB is running to the configuration table
	addEntry("Port", config.Port)

	// Add whether multi-threading is enabled to the configuration table
	addEntry("Multi Threading Enabled", config.EnableMultiThreading)

	// Add the number of CPU cores available on the machine to the configuration table
	addEntry("Cores", runtime.NumCPU())

	// Conditionally add the number of shards to be used for DiceDB to the configuration table
	if config.EnableMultiThreading {
		if config.NumShards > 0 {
			configTable = append(configTable, configEntry{"Shards", config.NumShards})
		} else {
			configTable = append(configTable, configEntry{"Shards", runtime.NumCPU()})
		}
	} else {
		configTable = append(configTable, configEntry{"Shards", 1})
	}

	// Add whether the watch feature is enabled to the configuration table
	addEntry("Watch Enabled", config.EnableWatch)

	// Add whether the watch feature is enabled to the configuration table
	addEntry("HTTP Enabled", config.EnableHTTP)

	// Add whether the watch feature is enabled to the configuration table
	addEntry("Websocket Enabled", config.EnableWebsocket)
}

func addEntry(k string, v interface{}) {
	configTable = append(configTable, configEntry{k, v})
}

// printConfigTable prints key-value pairs in a vertical table format.
func printConfigTable() {
	configuration()

	// Find the longest key to align the values properly
	maxKeyLength := 0
	maxValueLength := 20 // Default value length for alignment
	for _, entry := range configTable {
		if len(entry.Key) > maxKeyLength {
			maxKeyLength = len(entry.Key)
		}
		if len(fmt.Sprintf("%v", entry.Value)) > maxValueLength {
			maxValueLength = len(fmt.Sprintf("%v", entry.Value))
		}
	}

	// Create the table header and separator line
	fmt.Println()
	totalWidth := maxKeyLength + maxValueLength + 7 // 7 is for spacing and pipes
	fmt.Println(strings.Repeat("-", totalWidth))
	fmt.Printf("| %-*s | %-*s |\n", maxKeyLength, "Configuration", maxValueLength, "Value")
	fmt.Println(strings.Repeat("-", totalWidth))

	// Print each configuration key-value pair without row lines
	for _, entry := range configTable {
		fmt.Printf("| %-*s | %-20v |\n", maxKeyLength, entry.Key, entry.Value)
	}

	// Final bottom line
	fmt.Println(strings.Repeat("-", totalWidth))
	fmt.Println()
}

func main() {
	printSplash()
	printConfigTable()

	go observability.Ping()

	ctx, cancel := context.WithCancel(context.Background())

	// Handle SIGTERM and SIGINT
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)

	var (
		queryWatchChan           chan dstore.QueryWatchEvent
		cmdWatchChan             chan dstore.CmdWatchEvent
		serverErrCh              = make(chan error, 2)
		cmdWatchSubscriptionChan = make(chan watchmanager.WatchSubscription)
		wl                       wal.AbstractWAL
	)

	wl, _ = wal.NewNullWAL()
	slog.Info("running with", slog.Bool("enable-wal", config.EnableWAL))
	if config.EnableWAL {
		if config.WALEngine == "sqlite" {
			_wl, err := wal.NewSQLiteWAL(config.LogDir)
			if err != nil {
				slog.Warn("could not create WAL with", slog.String("wal-engine", config.WALEngine), slog.Any("error", err))
				sigs <- syscall.SIGKILL
				return
			}
			wl = _wl
		} else if config.WALEngine == "aof" {
			_wl, err := wal.NewAOFWAL(config.LogDir)
			if err != nil {
				slog.Warn("could not create WAL with", slog.String("wal-engine", config.WALEngine), slog.Any("error", err))
				sigs <- syscall.SIGKILL
				return
			}
			wl = _wl
		} else {
			slog.Error("unsupported WAL engine", slog.String("engine", config.WALEngine))
			sigs <- syscall.SIGKILL
			return
		}

		if err := wl.Init(time.Now()); err != nil {
			slog.Error("could not initialize WAL", slog.Any("error", err))
		} else {
			go wal.InitBG(wl)
		}

		slog.Debug("WAL initialization complete")

		if config.RestoreFromWAL {
			slog.Info("restoring database from WAL")
			wal.ReplayWAL(wl)
			slog.Info("database restored from WAL")
		}
	}

	if config.EnableWatch {
		bufSize := config.DiceConfig.Performance.WatchChanBufSize
		queryWatchChan = make(chan dstore.QueryWatchEvent, bufSize)
		cmdWatchChan = make(chan dstore.CmdWatchEvent, bufSize)
	}

	// Get the number of available CPU cores on the machine using runtime.NumCPU().
	// This determines the total number of logical processors that can be utilized
	// for parallel execution. Setting the maximum number of CPUs to the available
	// core count ensures the application can make full use of all available hardware.
	// If multithreading is not enabled, server will run on a single core.
	var numShards int
	if config.EnableMultiThreading {
		numShards = runtime.NumCPU()
		if config.NumShards > 0 {
			numShards = config.NumShards
		}
	} else {
		numShards = 1
	}

	// The runtime.GOMAXPROCS(numShards) call limits the number of operating system
	// threads that can execute Go code simultaneously to the number of CPU cores.
	// This enables Go to run more efficiently, maximizing CPU utilization and
	// improving concurrency performance across multiple goroutines.
	runtime.GOMAXPROCS(runtime.NumCPU())

	// Initialize the ShardManager
	shardManager := shard.NewShardManager(uint8(numShards), queryWatchChan, cmdWatchChan, serverErrCh)

	wg := sync.WaitGroup{}

	wg.Add(1)
	go func() {
		defer wg.Done()
		shardManager.Run(ctx)
	}()

	var serverWg sync.WaitGroup

	if config.EnableMultiThreading {
		if config.EnableProfiling {
			stopProfiling, err := startProfiling()
			if err != nil {
				slog.Error("Profiling could not be started", slog.Any("error", err))
				sigs <- syscall.SIGKILL
			}
			defer stopProfiling()
		}

		workerManager := worker.NewWorkerManager(config.DiceConfig.Performance.MaxClients, shardManager)
		respServer := resp.NewServer(shardManager, workerManager, cmdWatchSubscriptionChan, cmdWatchChan, serverErrCh, wl)
		serverWg.Add(1)
		go runServer(ctx, &serverWg, respServer, serverErrCh)
	} else {
		asyncServer := server.NewAsyncServer(shardManager, queryWatchChan, wl)
		if err := asyncServer.FindPortAndBind(); err != nil {
			slog.Error("Error finding and binding port", slog.Any("error", err))
			sigs <- syscall.SIGKILL
		}

		serverWg.Add(1)
		go runServer(ctx, &serverWg, asyncServer, serverErrCh)

		if config.EnableHTTP {
			httpServer := server.NewHTTPServer(shardManager, wl)
			serverWg.Add(1)
			go runServer(ctx, &serverWg, httpServer, serverErrCh)
		}
	}

	if config.EnableWebsocket {
		websocketServer := server.NewWebSocketServer(shardManager, config.WebsocketPort, wl)
		serverWg.Add(1)
		go runServer(ctx, &serverWg, websocketServer, serverErrCh)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-sigs
		cancel()
	}()

	go func() {
		serverWg.Wait()
		close(serverErrCh) // Close the channel when both servers are done
	}()

	for err := range serverErrCh {
		if err != nil && errors.Is(err, diceerrors.ErrAborted) {
			// if either the AsyncServer/RESPServer or the HTTPServer received an abort command,
			// cancel the context, helping gracefully exiting all servers
			cancel()
		}
	}

	close(sigs)

	if config.EnableWAL {
		wal.ShutdownBG()
	}

	cancel()

	wg.Wait()
}

func runServer(ctx context.Context, wg *sync.WaitGroup, srv abstractserver.AbstractServer, errCh chan<- error) {
	defer wg.Done()
	if err := srv.Run(ctx); err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			slog.Debug(fmt.Sprintf("%T was canceled", srv))
		case errors.Is(err, diceerrors.ErrAborted):
			slog.Debug(fmt.Sprintf("%T received abort command", srv))
		case errors.Is(err, http.ErrServerClosed):
			slog.Debug(fmt.Sprintf("%T received abort command", srv))
		default:
			slog.Error(fmt.Sprintf("%T error", srv), slog.Any("error", err))
		}
		errCh <- err
	} else {
		slog.Debug("bye.")
	}
}
func startProfiling() (func(), error) {
	// Start CPU profiling
	cpuFile, err := os.Create("cpu.prof")
	if err != nil {
		return nil, fmt.Errorf("could not create cpu.prof: %w", err)
	}

	if err = pprof.StartCPUProfile(cpuFile); err != nil {
		cpuFile.Close()
		return nil, fmt.Errorf("could not start CPU profile: %w", err)
	}

	// Start memory profiling
	memFile, err := os.Create("mem.prof")
	if err != nil {
		pprof.StopCPUProfile()
		cpuFile.Close()
		return nil, fmt.Errorf("could not create mem.prof: %w", err)
	}

	// Start block profiling
	runtime.SetBlockProfileRate(1)

	// Start execution trace
	traceFile, err := os.Create("trace.out")
	if err != nil {
		runtime.SetBlockProfileRate(0)
		memFile.Close()
		pprof.StopCPUProfile()
		cpuFile.Close()
		return nil, fmt.Errorf("could not create trace.out: %w", err)
	}

	if err := trace.Start(traceFile); err != nil {
		traceFile.Close()
		runtime.SetBlockProfileRate(0)
		memFile.Close()
		pprof.StopCPUProfile()
		cpuFile.Close()
		return nil, fmt.Errorf("could not start trace: %w", err)
	}

	// Return a cleanup function
	return func() {
		// Stop the CPU profiling and close cpuFile
		pprof.StopCPUProfile()
		cpuFile.Close()

		// Write heap profile
		runtime.GC()
		if err := pprof.WriteHeapProfile(memFile); err != nil {
			slog.Warn("could not write memory profile", slog.Any("error", err))
		}

		memFile.Close()

		// Write block profile
		blockFile, err := os.Create("block.prof")
		if err != nil {
			slog.Warn("could not create block profile", slog.Any("error", err))
		} else {
			if err := pprof.Lookup("block").WriteTo(blockFile, 0); err != nil {
				slog.Warn("could not write block profile", slog.Any("error", err))
			}
			blockFile.Close()
		}

		runtime.SetBlockProfileRate(0)

		// Stop trace and close traceFile
		trace.Stop()
		traceFile.Close()
	}, nil
}
