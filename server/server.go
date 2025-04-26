package chserver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jpillora/chisel/dcrpc"
	chshare "github.com/jpillora/chisel/share"
	"github.com/jpillora/chisel/share/ccrypto"
	"github.com/jpillora/chisel/share/cio"
	"github.com/jpillora/chisel/share/cnet"
	"github.com/jpillora/chisel/share/craveauth"
	"github.com/jpillora/chisel/share/settings"
	"github.com/jpillora/requestlog"
	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
)

// Config is the configuration for the chisel service
type Config struct {
	KeySeed      string
	AuthFile     string
	Auth         string
	Proxy        string
	Socks5       bool
	Reverse      bool
	KeepAlive    time.Duration
	TLS          TLSConfig
	DCMasterPort string
}

type DynamicReverseProxy struct {
	Handler        *httputil.ReverseProxy
	AuthKey        []byte
	Target         string
	User           int64
	JobId          int64
	ServicePrefix  string
	ProxyType      string
	DcMasterClient dcrpc.DcMasterRPCClient
	GrpcConn       *grpc.ClientConn // TODO: Put this into an interface
}

// Server respresent a chisel service
type Server struct {
	*cio.Logger
	config                *Config
	fingerprint           string
	httpServer            *cnet.HTTPServer
	reverseProxy          *httputil.ReverseProxy
	dynamicReverseProxies map[string]*DynamicReverseProxy
	sessCount             int32
	sessions              *settings.Users
	sshConfig             *ssh.ServerConfig
	users                 *settings.UserIndex
}

var upgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  settings.EnvInt("WS_BUFF_SIZE", 0),
	WriteBufferSize: settings.EnvInt("WS_BUFF_SIZE", 0),
}

func GetDCMasterPort(l *cio.Logger) (dcMasterPort string, err error) {
	dbIP := os.Getenv("DB_HOST")
	if len(dbIP) == 0 {
		l.Infof("could not get DB_HOST")
		return
	}
	dbUser := os.Getenv("DB_USER")
	if len(dbUser) == 0 {
		l.Infof("could not get DB_USER")
		return
	}
	dbPass := os.Getenv("DB_PASS")
	if len(dbPass) == 0 {
		l.Infof("could not get DB_PASS")
		return
	}
	dbName := os.Getenv("DB_NAME")
	if len(dbName) == 0 {
		l.Infof("could not get DB_NAME")
		return
	}

	pgString := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", dbUser, dbPass, dbIP, dbName)

	// urlExample := "postgres://username:password@localhost:5432/database_name"
	conn, err := pgx.Connect(context.Background(), pgString)
	if err != nil {
		l.Infof("Unable to connect to database: %v", err)
		return
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(context.Background(), "SELECT \"Value\" FROM build_deploymentsetting where \"Key\" = 'DCMASTER_PORT';")
	if err != nil {
		l.Infof("Query failed: %v", err)
		return
	}

	for rows.Next() {
		err = rows.Scan(&dcMasterPort)
		if err != nil {
			l.Infof("Row scanning failed: %v", err)
			return
		}
		defer rows.Close()
	}
	if err = rows.Err(); err != nil {
		l.Infof("rows error: %v", err)
		return
	}
	return
}

// NewServer creates and returns a new chisel server
func NewServer(c *Config) (*Server, error) {
	server := &Server{
		config:     c,
		httpServer: cnet.NewHTTPServer(),
		Logger:     cio.NewLogger("server"),
		sessions:   settings.NewUsers(),
	}
	server.Info = true
	server.users = settings.NewUserIndex(server.Logger)
	if c.AuthFile != "" {
		if err := server.users.LoadUsers(c.AuthFile); err != nil {
			return nil, err
		}
	}
	if c.Auth != "" {
		u := &settings.User{Addrs: []*regexp.Regexp{settings.UserAllowAll}}
		u.Name, u.Pass = settings.ParseAuth(c.Auth)
		if u.Name != "" {
			server.users.AddUser(u)
		}
		log.Printf("Users init %v", server.users)
	}
	//generate private key (optionally using seed)
	key, err := ccrypto.GenerateKey(c.KeySeed)
	if err != nil {
		log.Fatal("Failed to generate key")
	}
	//convert into ssh.PrivateKey
	private, err := ssh.ParsePrivateKey(key)
	if err != nil {
		log.Fatal("Failed to parse key")
	}
	//fingerprint this key
	server.fingerprint = ccrypto.FingerprintKey(private.PublicKey())
	//create ssh config
	server.sshConfig = &ssh.ServerConfig{
		ServerVersion:    "SSH-" + chshare.ProtocolVersion + "-server",
		PasswordCallback: server.authUser,
	}
	server.sshConfig.AddHostKey(private)
	//setup reverse proxy
	if c.Proxy != "" {
		u, err := url.Parse(c.Proxy)
		if err != nil {
			return nil, err
		}
		if u.Host == "" {
			return nil, server.Errorf("Missing protocol (%s)", u)
		}
		server.reverseProxy = httputil.NewSingleHostReverseProxy(u)
		//always use proxy host
		server.reverseProxy.Director = func(r *http.Request) {
			//enforce origin, keep path
			r.URL.Scheme = u.Scheme
			r.URL.Host = u.Host
			r.Host = u.Host
		}
	}
	c.DCMasterPort, err = GetDCMasterPort(server.Logger)
	if len(c.DCMasterPort) == 0 || err != nil {
		return nil, server.Errorf("Failed to get DCMasterPort. Error: %v", err)
	}
	server.Infof("Got dcmaster port: %v", c.DCMasterPort)
	server.dynamicReverseProxies = make(map[string]*DynamicReverseProxy)
	//print when reverse tunnelling is enabled
	if c.Reverse {
		server.Infof("Reverse tunnelling enabled")
	}
	return server, nil
}

// Run is responsible for starting the chisel service.
// Internally this calls Start then Wait.
func (s *Server) Run(host, port string) error {
	if err := s.Start(host, port); err != nil {
		return err
	}
	return s.Wait()
}

// Start is responsible for kicking off the http server
func (s *Server) Start(host, port string) error {
	return s.StartContext(context.Background(), host, port)
}

// StartContext is responsible for kicking off the http server,
// and can be closed by cancelling the provided context
func (s *Server) StartContext(ctx context.Context, host, port string) error {
	s.Infof("Fingerprint %s", s.fingerprint)
	if s.users.Len() > 0 {
		s.Infof("User authentication enabled")
	}
	if s.reverseProxy != nil {
		s.Infof("Reverse proxy enabled")
	}
	l, err := s.listener(host, port)
	if err != nil {
		return err
	}
	h := http.Handler(http.HandlerFunc(s.handleClientHandler))
	if s.Debug {
		o := requestlog.DefaultOptions
		o.TrustProxy = true
		h = requestlog.WrapWith(h, o)
	}
	return s.httpServer.GoServe(ctx, l, h)
}

// Wait waits for the http server to close
func (s *Server) Wait() error {
	return s.httpServer.Wait()
}

// Close forcibly closes the http server
func (s *Server) Close() error {
	return s.httpServer.Close()
}

// GetFingerprint is used to access the server fingerprint
func (s *Server) GetFingerprint() string {
	return s.fingerprint
}

// authUser is responsible for validating the ssh user / password combination
func (s *Server) authUser(c ssh.ConnMetadata, password []byte) (p *ssh.Permissions, err error) {
	// check if user authentication is enabled and if not, allow all
	if s.users.Len() == 0 {
		return nil, nil
	}

	p, err = craveauth.Auth(c, password, s.Logger)

	if err == nil {
		n := c.User()
		user, found := s.users.Get("all")
		if !found || user.Pass != string("all") {
			s.Infof("Login failed for user: %s", n)
			err = errors.New("Invalid authentication for username: %s")
		} else {
			s.Infof("Login success for user: %s", n)
			s.sessions.Set(string(c.SessionID()), user)
		}
	}
	return

}

// AddUser adds a new user into the server user index
func (s *Server) AddUser(user, pass string, addrs ...string) error {
	authorizedAddrs := []*regexp.Regexp{}
	for _, addr := range addrs {
		authorizedAddr, err := regexp.Compile(addr)
		if err != nil {
			return err
		}
		authorizedAddrs = append(authorizedAddrs, authorizedAddr)
	}
	s.users.AddUser(&settings.User{
		Name:  user,
		Pass:  pass,
		Addrs: authorizedAddrs,
	})
	return nil
}

// DeleteUser removes a user from the server user index
func (s *Server) DeleteUser(user string) {
	s.users.Del(user)
}

// ResetUsers in the server user index.
// Use nil to remove all.
func (s *Server) ResetUsers(users []*settings.User) {
	s.users.Reset(users)
}
