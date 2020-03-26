package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof" // register in DefaultServerMux
	"os"
	"time"
	"encoding/json"

	"crypto/tls"

	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_logrus "github.com/grpc-ecosystem/go-grpc-middleware/logging/logrus"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/dshuffma-ibm/grpc-web/go/grpcweb"
	"github.com/mwitkow/go-conntrack"
	"github.com/mwitkow/grpc-proxy/proxy"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"golang.org/x/net/context"
	_ "golang.org/x/net/trace" // register in DefaultServerMux
	"google.golang.org/grpc"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/metadata"
)

var (
	flagBindAddr    = pflag.String("server_bind_address", "0.0.0.0", "address to bind the server to")
	flagHttpPort    = pflag.Int("server_http_debug_port", 8080, "TCP port to listen on for HTTP1.1 debug calls.")
	flagHttpTlsPort = pflag.Int("server_http_tls_port", 8443, "TCP port to listen on for HTTPS (gRPC, gRPC-Web).")

	flagAllowAllOrigins = pflag.Bool("allow_all_origins", false, "allow requests from any origin.")
	flagAllowedOrigins  = pflag.StringSlice("allowed_origins", nil, "comma-separated list of origin URLs which are allowed to make cross-origin requests.")

	runHttpServer = pflag.Bool("run_http_server", true, "whether to run HTTP server")
	runTlsServer  = pflag.Bool("run_tls_server", true, "whether to run TLS server")

	useWebsockets = pflag.Bool("use_websockets", false, "whether to use beta websocket transport layer")

	flagHttpMaxWriteTimeout = pflag.Duration("server_http_max_write_timeout", 10*time.Second, "HTTP server config, max write duration.")
	flagHttpMaxReadTimeout  = pflag.Duration("server_http_max_read_timeout", 10*time.Second, "HTTP server config, max read duration.")
)

func main() {
	pflag.Parse()

	logrus.SetOutput(os.Stdout)
	logEntry := logrus.NewEntry(logrus.StandardLogger())

	if *flagAllowAllOrigins && len(*flagAllowedOrigins) != 0 {
		logrus.Fatal("Ambiguous --allow_all_origins and --allow_origins configuration. Either set --allow_all_origins=true OR specify one or more origins to whitelist with --allow_origins, not both.")
	}

	grpcServer := buildGrpcProxyServer(logEntry)
	errChan := make(chan error)

	allowedOrigins := makeAllowedOrigins(*flagAllowedOrigins)

	options := []grpcweb.Option{
		grpcweb.WithCorsForRegisteredEndpointsOnly(false),
		grpcweb.WithOriginFunc(makeHttpOriginFunc(allowedOrigins)),
	}

	if *useWebsockets {
		logrus.Println("using websockets")
		options = append(
			options,
			grpcweb.WithWebsockets(true),
			grpcweb.WithWebsocketOriginFunc(makeWebsocketOriginFunc(allowedOrigins)),
		)
	}
	wrappedGrpc := grpcweb.WrapServer(grpcServer, options...)

	if !*runHttpServer && !*runTlsServer {
		logrus.Fatalf("Both run_http_server and run_tls_server are set to false. At least one must be enabled for grpcweb proxy to function correctly.")
	}

	var theSettings Settings = buildSettings();
	jsonData, _ := json.Marshal(theSettings)
	logrus.Printf("version: %s", theSettings.Version)
	logrus.Printf("grpc web proxy configuration settings: %s", jsonData)

	http.Handle("/", wrappedGrpc)
	http.HandleFunc("/settings", leakSettings)

	if *runHttpServer {
		// Debug server.
		debugServer := buildServer(http.DefaultServeMux)
		http.Handle("/metrics", promhttp.Handler())
		debugListener := buildListenerOrFail("http", *flagHttpPort)
		serveServer(debugServer, debugListener, "http", errChan)
	}

	if *runTlsServer {
		// tls server.
		//servingServer := buildServer(wrappedGrpc)
		servingServer := buildServer(http.DefaultServeMux)
		servingListener := buildListenerOrFail("http", *flagHttpTlsPort)
		servingListener = tls.NewListener(servingListener, buildServerTlsOrFail())
		serveServer(servingServer, servingListener, "http_tls", errChan)
	}

	<-errChan
	// TODO(mwitkow): Add graceful shutdown.
}

// build the settings to print out later
type Settings struct {
	// defined in main.go
	BackendAddr string `json:"backend_addr"`
	BackendTLS bool `json:"backend_tls"`
	BackendTLSNoVerify bool `json:"backend_tls_noverify"`
	FlagBackendTlsClientCert string `json:"backend_client_tls_cert_file"`
	BackendMaxCallRecvMsgSize int `json:"backend_max_call_recv_msg_size_bytes"`
	FlagBackendTlsCa []string `json:"backend_tls_ca_files"`
	FlagBackendDefaultAuthority string `json:"backend_default_authority"`
	KeepAliveClientInterval time.Duration `json:"keep_alive_client_interval_ns"`
	KeepAliveClientTimeout time.Duration `json:"keep_alive_client_timeout_ns"`
	ExternalAddr string `json:"external_addr"`
	FlagBackendBackoffMaxDelay time.Duration `json:"flag_backend_backoff_max_delay_ns"`

	// defined in backend.go
	FlagBindAddr string `json:"server_bind_address"`
	FlagAllowAllOrigins bool `json:"allow_all_origins"`
	FlagAllowedOrigins []string `json:"allowed_origins"`
	RunHTTPServer bool `json:"run_http_server"`
	RunTLSServer bool `json:"run_tls_server"`
	UseWebSockets bool `json:"use_websockets"`
	ServerHttpMaxWriteTimeout time.Duration `json:"server_http_max_write_timeout_ns"`
	ServerHttpMaxReadTimeout time.Duration `json:"server_http_max_read_timeout_ns"`

	// defined in server_tls.go
	FlagTlsServerCert string `json:"server_tls_cert_file"`
	FlagTlsServerClientCertVerification string `json:"server_tls_client_cert_verification"`
	FlagTlsServerClientCAFiles []string `json:"server_tls_client_ca_files"`

	// version of the grpc web proxy, hard coded
	Version string `json:"version"`
}
func buildSettings() Settings{
	var theSettings Settings
	theSettings.BackendAddr = *flagBackendHostPort
	theSettings.BackendTLS = *flagBackendIsUsingTls
	theSettings.BackendTLSNoVerify = *flagBackendTlsNoVerify
	theSettings.FlagBackendTlsClientCert = *flagBackendTlsClientCert
	theSettings.BackendMaxCallRecvMsgSize = *flagMaxCallRecvMsgSize
	theSettings.FlagBackendTlsCa = *flagBackendTlsCa
	theSettings.FlagBackendDefaultAuthority = *flagBackendDefaultAuthority
	theSettings.KeepAliveClientInterval = *flagKeepAliveClientInterval
	theSettings.KeepAliveClientTimeout = *flagKeepAliveClientTimeout
	theSettings.ExternalAddr = *flagExternalHostPort
	theSettings.FlagBackendBackoffMaxDelay = *flagBackendBackoffMaxDelay

	theSettings.FlagBindAddr = *flagBindAddr
	theSettings.FlagAllowAllOrigins = *flagAllowAllOrigins
	theSettings.FlagAllowedOrigins = *flagAllowedOrigins
	theSettings.RunHTTPServer = *runHttpServer
	theSettings.RunTLSServer = *runTlsServer
	theSettings.UseWebSockets = *useWebsockets
	theSettings.ServerHttpMaxWriteTimeout = *flagHttpMaxWriteTimeout
	theSettings.ServerHttpMaxReadTimeout = *flagHttpMaxReadTimeout

	theSettings.FlagTlsServerCert = *flagTlsServerCert
	theSettings.FlagTlsServerClientCertVerification = *flagTlsServerClientCertVerification
	theSettings.FlagTlsServerClientCAFiles = *flagTlsServerClientCAFiles

	theSettings.Version = "v0.11.0-1"

	if theSettings.ExternalAddr == "" {
		theSettings.ExternalAddr = theSettings.BackendAddr // if external doesn't exist, show internal address
	}
	return theSettings;
}

// send current settings to http output
func leakSettings(w http.ResponseWriter, r *http.Request) {
	var theSettings Settings = buildSettings();
	jsonData, _ := json.Marshal(theSettings)

	w.Header().Set("Content-Type", "application/json")
	w.Write(jsonData)
}

//func buildServer(wrappedGrpc *grpcweb.WrappedGrpcServer) *http.Server {
func buildServer(handler http.Handler) *http.Server {
	return &http.Server{
		WriteTimeout: *flagHttpMaxWriteTimeout,
		ReadTimeout:  *flagHttpMaxReadTimeout,
		/*Handler: http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
			wrappedGrpc.ServeHTTP(resp, req)
		}),*/
		Handler:      handler,
	}
}

func serveServer(server *http.Server, listener net.Listener, name string, errChan chan error) {
	go func() {
		logrus.Infof("listening for %s on: %v", name, listener.Addr().String())
		if err := server.Serve(listener); err != nil {
			errChan <- fmt.Errorf("%s server error: %v", name, err)
		}
	}()
}

func buildGrpcProxyServer(logger *logrus.Entry) *grpc.Server {
	// gRPC-wide changes.
	grpc.EnableTracing = true
	grpc_logrus.ReplaceGrpcLogger(logger)

	// gRPC proxy logic.
	backendConn := dialBackendOrFail()
	director := func(ctx context.Context, fullMethodName string) (context.Context, *grpc.ClientConn, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		outCtx, _ := context.WithCancel(ctx)
		mdCopy := md.Copy()
		delete(mdCopy, "user-agent")
		outCtx = metadata.NewOutgoingContext(outCtx, mdCopy)
		return outCtx, backendConn, nil
	}
	// Server with logging and monitoring enabled.
	return grpc.NewServer(
		grpc.CustomCodec(proxy.Codec()), // needed for proxy to function.
		grpc.UnknownServiceHandler(proxy.TransparentHandler(director)),
		grpc.MaxRecvMsgSize(*flagMaxCallRecvMsgSize),
		grpc_middleware.WithUnaryServerChain(
			grpc_logrus.UnaryServerInterceptor(logger),
			grpc_prometheus.UnaryServerInterceptor,
		),
		grpc_middleware.WithStreamServerChain(
			grpc_logrus.StreamServerInterceptor(logger),
			grpc_prometheus.StreamServerInterceptor,
		),
	)
}

func buildListenerOrFail(name string, port int) net.Listener {
	addr := fmt.Sprintf("%s:%d", *flagBindAddr, port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("failed listening for '%v' on %v: %v", name, port, err)
	}
	return conntrack.NewListener(listener,
		conntrack.TrackWithName(name),
		conntrack.TrackWithTcpKeepAlive(20*time.Second),
		conntrack.TrackWithTracing(),
	)
}

func makeHttpOriginFunc(allowedOrigins *allowedOrigins) func(origin string) bool {
	if *flagAllowAllOrigins {
		return func(origin string) bool {
			return true
		}
	}
	return allowedOrigins.IsAllowed
}

func makeWebsocketOriginFunc(allowedOrigins *allowedOrigins) func(req *http.Request) bool {
	if *flagAllowAllOrigins {
		return func(req *http.Request) bool {
			return true
		}
	} else {
		return func(req *http.Request) bool {
			origin, err := grpcweb.WebsocketRequestOrigin(req)
			if err != nil {
				grpclog.Warning(err)
				return false
			}
			return allowedOrigins.IsAllowed(origin)
		}
	}
}

func makeAllowedOrigins(origins []string) *allowedOrigins {
	o := map[string]struct{}{}
	for _, allowedOrigin := range origins {
		o[allowedOrigin] = struct{}{}
	}
	return &allowedOrigins{
		origins: o,
	}
}

type allowedOrigins struct {
	origins map[string]struct{}
}

func (a *allowedOrigins) IsAllowed(origin string) bool {
	_, ok := a.origins[origin]
	return ok
}
