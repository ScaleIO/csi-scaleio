package provider

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/thecodeteam/goioc"
	"google.golang.org/grpc"

	"github.com/thecodeteam/gocsi"
	"github.com/thecodeteam/gocsi/csi"
	"github.com/thecodeteam/gocsi/mock/service"
)

const (
	// ReqLoggingEnabled is the name of the environment variable
	// used to determine if the Mock server should enable request
	// logging.
	ReqLoggingEnabled = "X_CSI_MOCK_REQ_LOGGING_ENABLED"

	// RepLoggingEnabled is the name of the environment variable
	// used to determine if the Mock server should enable response
	// logging.
	RepLoggingEnabled = "X_CSI_MOCK_REP_LOGGING_ENABLED"

	// ReqIDInjectionEnabled is the name of the environment variable
	// used to determine if the Mock server should enable request ID
	// injection.
	ReqIDInjectionEnabled = "X_CSI_MOCK_REQ_ID_INJECTION_ENABLED"

	// SpecValidationEnabled is the name of the environment variable
	// used to determine if the Mock server should enable request
	// specification validation.
	SpecValidationEnabled = "X_CSI_MOCK_SPEC_VALIDATION_ENABLED"

	// IdempEnabled is the name of the environment variable
	// used to determine if the Mock server should enable idempotency.
	IdempEnabled = "X_CSI_MOCK_IDEMPOTENCY_ENABLED"

	// IdempTimeout is the name of the environment variable
	// used to obtain the time.Duration that is the timeout
	// for this plug-in's idempotency provider.
	IdempTimeout = "X_CSI_MOCK_IDEMPOTENCY_TIMEOUT"

	// IdempRequireVolume is the name of the environment variable
	// used to determine if the idempotency provider checks to
	// see if a volume exists prior to acting upon it.
	IdempRequireVolume = "X_CSI_MOCK_IDEMPOTENCY_REQUIRE_VOLUME"
)

var (
	errServerStopped = errors.New("server stopped")
	errServerStarted = errors.New("server started")
)

// ServiceProvider is a gRPC endpoint that provides the CSI
// services: Controller, Identity, Node.
type ServiceProvider interface {

	// Serve accepts incoming connections on the listener lis, creating
	// a new ServerTransport and service goroutine for each. The service
	// goroutine read gRPC requests and then call the registered handlers
	// to reply to them. Serve returns when lis.Accept fails with fatal
	// errors.  lis will be closed when this method returns.
	// Serve always returns non-nil error.
	Serve(ctx context.Context, lis net.Listener) error

	// Stop stops the gRPC server. It immediately closes all open
	// connections and listeners.
	// It cancels all active RPCs on the server side and the corresponding
	// pending RPCs on the client side will get notified by connection
	// errors.
	Stop(ctx context.Context)

	// GracefulStop stops the gRPC server gracefully. It stops the server
	// from accepting new connections and RPCs and blocks until all the
	// pending RPCs are finished.
	GracefulStop(ctx context.Context)
}

func init() {
	goioc.Register(service.Name, func() interface{} { return &provider{} })
}

// New returns a new service provider.
func New() ServiceProvider {
	return &provider{}
}

type provider struct {
	sync.Mutex
	server  *grpc.Server
	closed  bool
	service service.Service
}

var (
	// ctxOSEnvironKey is an interface-wrapped key used to access a possible
	// environment variable slice given to the provider's Serve function
	ctxOSEnvironKey = interface{}("os.Environ")

	// ctxOSGetenvKey is an interface-wrapped key used to access a function
	// with the signature func(string)string that returns the value of an
	// environment variable.
	ctxOSGetenvKey = interface{}("os.Getenev")
)

// getEnvFunc is the function signature for os.Getenv.
type getEnvFunc func(string) string

// Serve accepts incoming connections on the listener lis, creating
// a new ServerTransport and service goroutine for each. The service
// goroutine read gRPC requests and then call the registered handlers
// to reply to them. Serve returns when lis.Accept fails with fatal
// errors.  lis will be closed when this method returns.
// Serve always returns non-nil error.
func (p *provider) Serve(ctx context.Context, li net.Listener) error {

	// Assign the provider a new Mock plug-in.
	p.service = service.New()

	// Create a new gRPC server for serving the storage plug-in.
	if err := func() error {
		p.Lock()
		defer p.Unlock()
		if p.closed {
			return errServerStopped
		}
		if p.server != nil {
			return errServerStarted
		}
		p.server = p.newGrpcServer(ctx, p.service)
		return nil
	}(); err != nil {
		return errServerStarted
	}

	// Register the services.
	csi.RegisterControllerServer(p.server, p.service)
	csi.RegisterIdentityServer(p.server, p.service)
	csi.RegisterNodeServer(p.server, p.service)

	// Start the grpc server
	log.WithFields(map[string]interface{}{
		"service": service.Name,
		"address": fmt.Sprintf(
			"%s://%s", li.Addr().Network(), li.Addr().String()),
	}).Info("serving")

	return p.server.Serve(li)
}

// Stop stops the gRPC server. It immediately closes all open
// connections and listeners.
// It cancels all active RPCs on the server side and the corresponding
// pending RPCs on the client side will get notified by connection
// errors.
func (p *provider) Stop(ctx context.Context) {
	if p.server == nil {
		return
	}
	p.Lock()
	defer p.Unlock()
	p.server.Stop()
	p.closed = true
	log.WithField("service", service.Name).Info("stopped")
}

// GracefulStop stops the gRPC server gracefully. It stops the server
// from accepting new connections and RPCs and blocks until all the
// pending RPCs are finished.
func (p *provider) GracefulStop(ctx context.Context) {
	if p.server == nil {
		return
	}
	p.Lock()
	defer p.Unlock()
	p.server.GracefulStop()
	p.closed = true
	log.WithField("service", service.Name).Info("shutdown")
}

func (p *provider) newGrpcServer(
	ctx context.Context,
	i gocsi.IdempotencyProvider) *grpc.Server {

	// Get the function used to query environment variables.
	var getEnv = os.Getenv
	if f, ok := ctx.Value(ctxOSGetenvKey).(getEnvFunc); ok {
		getEnv = f
	}

	// Create the server-side interceptor chain option.
	iceptors := newGrpcInterceptors(ctx, i, getEnv)
	iceptorChain := gocsi.ChainUnaryServer(iceptors...)
	iceptorOpt := grpc.UnaryInterceptor(iceptorChain)

	return grpc.NewServer(iceptorOpt)
}

func newGrpcInterceptors(
	ctx context.Context,
	idemp gocsi.IdempotencyProvider,
	getEnv getEnvFunc) []grpc.UnaryServerInterceptor {

	// pb parses an environment variable into a boolean value.
	pb := func(n string) bool {
		b, err := strconv.ParseBool(getEnv(n))
		if err != nil {
			return true
		}
		return b
	}

	var (
		usi           []grpc.UnaryServerInterceptor
		reqLogEnabled = pb(ReqLoggingEnabled)
		repLogEnabled = pb(RepLoggingEnabled)
		reqIDEnabled  = pb(ReqIDInjectionEnabled)
		specEnabled   = pb(SpecValidationEnabled)
		idempEnabled  = pb(IdempEnabled)
		idempReqVol   = pb(IdempRequireVolume)
	)

	if reqIDEnabled {
		usi = append(usi, gocsi.NewServerRequestIDInjector())
	}

	// If request or response logging are enabled then create the loggers.
	if reqLogEnabled || repLogEnabled {
		var (
			opts []gocsi.LoggingOption
			lout = newLogger(log.Infof)
		)
		if reqLogEnabled {
			opts = append(opts, gocsi.WithRequestLogging(lout))
		}
		if repLogEnabled {
			opts = append(opts, gocsi.WithResponseLogging(lout))
		}
		usi = append(usi, gocsi.NewServerLogger(opts...))
	}

	if specEnabled {
		sv := make([]csi.Version, len(service.SupportedVersions))
		for i, v := range service.SupportedVersions {
			sv[i] = *v
		}
		usi = append(
			usi,
			gocsi.NewServerSpecValidator(
				gocsi.WithSupportedVersions(sv...),
				gocsi.WithSuccessDeleteVolumeNotFound(),
				gocsi.WithSuccessCreateVolumeAlreadyExists(),
				gocsi.WithRequiresNodeID(),
				gocsi.WithRequiresPublishVolumeInfo(),
			),
		)
	}

	if idempEnabled {
		// Get idempotency provider's timeout.
		timeout, _ := time.ParseDuration(getEnv(IdempTimeout))

		iopts := []gocsi.IdempotentInterceptorOption{
			gocsi.WithIdempTimeout(timeout),
		}

		// Check to see if the idempotency provider requires volumes to exist.
		if idempReqVol {
			iopts = append(iopts, gocsi.WithIdempRequireVolumeExists())
		}

		usi = append(usi, gocsi.NewIdempotentInterceptor(idemp, iopts...))
	}

	return usi
}

type logger struct {
	f func(msg string, args ...interface{})
	w io.Writer
}

func newLogger(f func(msg string, args ...interface{})) *logger {
	l := &logger{f: f}
	r, w := io.Pipe()
	l.w = w
	go func() {
		scan := bufio.NewScanner(r)
		for scan.Scan() {
			f(scan.Text())
		}
	}()
	return l
}

func (l *logger) Write(data []byte) (int, error) {
	return l.w.Write(data)
}
