package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/Infisical/infisical-merge/packages/api"
	"github.com/go-resty/resty/v2"
	"github.com/pion/logging"
	"github.com/pion/turn/v4"
	"github.com/rs/zerolog/log"
)

type GatewayConfig struct {
	TurnServerUsername string
	TurnServerPassword string
	TurnServerAddress  string
	InfisicalStaticIp  string
	SerialNumber       string
	PrivateKey         string
	Certificate        string
	CertificateChain   string
}

type Gateway struct {
	httpClient *resty.Client
	config     *GatewayConfig
	client     *turn.Client
}

func NewGateway(identityToken string) (Gateway, error) {
	httpClient := resty.New()
	httpClient.SetAuthToken(identityToken)

	return Gateway{
		httpClient: httpClient,
		config:     &GatewayConfig{},
	}, nil
}

func (g *Gateway) ConnectWithRelay() error {
	relayDetails, err := api.CallRegisterGatewayIdentityV1(g.httpClient)
	if err != nil {
		return err
	}
	relayAddress, relayPort := strings.Split(relayDetails.TurnServerAddress, ":")[0], strings.Split(relayDetails.TurnServerAddress, ":")[1]
	var conn net.Conn

	// Dial TURN Server
	if relayPort == "5349" {
		log.Info().Msgf("Provided relay port %s. Using TLS", relayPort)
		conn, err = tls.Dial("tcp", relayDetails.TurnServerAddress, &tls.Config{
			InsecureSkipVerify: false,
			ServerName:         relayAddress,
		})
	} else {
		log.Info().Msgf("Provided relay port %s. Using non TLS connection.", relayPort)
		peerAddr, err := net.ResolveTCPAddr("tcp", relayDetails.TurnServerAddress)
		if err != nil {
			return fmt.Errorf("Failed to parse turn server address: %w", err)
		}
		conn, err = net.DialTCP("tcp", nil, peerAddr)
	}

	if err != nil {
		return fmt.Errorf("Failed to connect with relay server: %w", err)
	}

	// Start a new TURN Client and wrap our net.Conn in a STUNConn
	// This allows us to simulate datagram based communication over a net.Conn
	cfg := &turn.ClientConfig{
		STUNServerAddr: relayDetails.TurnServerAddress,
		TURNServerAddr: relayDetails.TurnServerAddress,
		Conn:           turn.NewSTUNConn(conn),
		Username:       relayDetails.TurnServerUsername,
		Password:       relayDetails.TurnServerPassword,
		Realm:          relayDetails.TurnServerRealm,
		LoggerFactory:  logging.NewDefaultLoggerFactory(),
	}

	client, err := turn.NewClient(cfg)
	if err != nil {
		return fmt.Errorf("Failed to create relay client: %w", err)
	}

	g.config = &GatewayConfig{
		TurnServerUsername: relayDetails.TurnServerUsername,
		TurnServerPassword: relayDetails.TurnServerPassword,
		TurnServerAddress:  relayDetails.TurnServerAddress,
		InfisicalStaticIp:  relayDetails.InfisicalStaticIp,
	}
	// if port not specific allow all port
	if relayDetails.InfisicalStaticIp != "" && !strings.Contains(relayDetails.InfisicalStaticIp, ":") {
		g.config.InfisicalStaticIp = g.config.InfisicalStaticIp + ":0"
	}

	g.client = client
	return nil
}

func (g *Gateway) Listen(ctx context.Context) error {
	defer g.client.Close()
	err := g.client.Listen()
	if err != nil {
		return fmt.Errorf("Failed to listen to relay server: %w", err)
	}

	log.Info().Msg("Connected with relay")

	// Allocate a relay socket on the TURN server. On success, it
	// will return a net.PacketConn which represents the remote
	// socket.
	relayNonTlsConn, err := g.client.AllocateTCP()
	if err != nil {
		return fmt.Errorf("Failed to allocate relay connection: %w", err)
	}

	log.Info().Msg(relayNonTlsConn.Addr().String())
	defer func() {
		if closeErr := relayNonTlsConn.Close(); closeErr != nil {
			log.Error().Msgf("Failed to close connection: %s", closeErr)
		}
	}()

	gatewayCert, err := api.CallExchangeRelayCertV1(g.httpClient, api.ExchangeRelayCertRequestV1{
		RelayAddress: relayNonTlsConn.Addr().String(),
	})
	if err != nil {
		return err
	}

	g.config.SerialNumber = gatewayCert.SerialNumber
	g.config.PrivateKey = gatewayCert.PrivateKey
	g.config.Certificate = gatewayCert.Certificate
	g.config.CertificateChain = gatewayCert.CertificateChain

	shutdownCh := make(chan bool, 1)

	if g.config.InfisicalStaticIp != "" {
		log.Info().Msgf("Found static ip from Infisical: %s. Creating permission IP lifecycle", g.config.InfisicalStaticIp)
		peerAddr, err := net.ResolveTCPAddr("tcp", g.config.InfisicalStaticIp)
		if err != nil {
			return fmt.Errorf("Failed to parse infisical static ip: %w", err)
		}
		g.registerPermissionLifecycle(func() error {
			err := relayNonTlsConn.CreatePermissions(peerAddr)
			return err
		}, shutdownCh)
	}

	cert, err := tls.X509KeyPair([]byte(gatewayCert.Certificate), []byte(gatewayCert.PrivateKey))
	if err != nil {
		return fmt.Errorf("failed to parse cert: %s", err)
	}

	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM([]byte(gatewayCert.CertificateChain))

	relayConn := tls.NewListener(relayNonTlsConn, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		ClientCAs:    caCertPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	})

	errCh := make(chan error, 1)
	log.Info().Msg("Gateway started successfully")
	g.registerHeartBeat(errCh, shutdownCh)
	g.registerRelayIsActive(relayNonTlsConn.Addr().String(), errCh, shutdownCh)

	// Create a WaitGroup to track active connections
	var wg sync.WaitGroup

	go func() {
		for {
			if relayDeadlineConn, ok := relayConn.(*net.TCPListener); ok {
				relayDeadlineConn.SetDeadline(time.Now().Add(1 * time.Second))
			}

			select {
			case <-ctx.Done():
				return
			case <-shutdownCh:
				return
			default:
				// Accept new relay connection
				conn, err := relayConn.Accept()
				if err != nil {
					// Check if it's a timeout error (which we expect due to our deadline)
					if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
						continue
					}

					if !strings.Contains(err.Error(), "data contains incomplete STUN or TURN frame") {
						log.Error().Msgf("Failed to accept connection: %v", err)
					}
					continue
				}

				tlsConn, ok := conn.(*tls.Conn)
				if !ok {
					log.Error().Msg("Failed to convert to TLS connection")
					conn.Close()
					continue
				}

				// Set a deadline for the handshake to prevent hanging
				tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
				err = tlsConn.Handshake()
				// Clear the deadline after handshake
				tlsConn.SetDeadline(time.Time{})
				if err != nil {
					log.Error().Msgf("TLS handshake failed: %v", err)
					conn.Close()
					continue
				}

				// Get connection state which contains certificate information
				state := tlsConn.ConnectionState()
				if len(state.PeerCertificates) > 0 {
					organizationUnit := state.PeerCertificates[0].Subject.OrganizationalUnit
					commonName := state.PeerCertificates[0].Subject.CommonName
					if organizationUnit[0] != "gateway-client" || commonName != "cloud" {
						log.Error().Msgf("Client certificate verification failed. Received %s, %s", organizationUnit, commonName)
						conn.Close()
						continue
					}
				}

				// Handle the connection in a goroutine
				wg.Add(1)
				go func(c net.Conn) {
					defer wg.Done()
					defer c.Close()

					// Monitor parent context to close this connection when needed
					go func() {
						select {
						case <-ctx.Done():
							c.Close() // Force close connection when context is canceled
						case <-shutdownCh:
							c.Close() // Force close connection when accepting loop is done
						}
					}()

					handleConnection(c)
				}(conn)
			}
		}
	}()

	select {
	case <-ctx.Done():
		log.Info().Msg("Shutting down gateway...")
	case err = <-errCh:
	}

	// Signal the accept loop to stop
	close(shutdownCh)

	// Set a timeout for waiting on connections to close
	waitCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitCh)
	}()

	select {
	case <-waitCh:
		// All connections closed normally
	case <-time.After(5 * time.Second):
		log.Warn().Msg("Timeout waiting for connections to close gracefully")
	}

	return err
}

func (g *Gateway) registerHeartBeat(errCh chan error, done chan bool) {
	ticker := time.NewTicker(1 * time.Hour)

	go func() {
		time.Sleep(10 * time.Second)
		log.Info().Msg("Registering first heart beat")
		err := api.CallGatewayHeartBeatV1(g.httpClient)
		if err != nil {
			log.Error().Msgf("Failed to register heartbeat: %s", err)
		}

		for {
			select {
			case <-done:
				ticker.Stop()
				return
			case <-ticker.C:
				log.Info().Msg("Registering heart beat")
				err := api.CallGatewayHeartBeatV1(g.httpClient)
				errCh <- err
			}
		}
	}()
}

func (g *Gateway) registerPermissionLifecycle(permissionFn func() error, done chan bool) {
	ticker := time.NewTicker(3 * time.Minute)

	go func() {
		// wait for 5 mins
		permissionFn()
		log.Printf("Created permission for incoming connections")
		for {
			select {
			case <-done:
				ticker.Stop()
				return
			case <-ticker.C:
				permissionFn()
			}
		}
	}()
}

func (g *Gateway) registerRelayIsActive(serverAddr string, errCh chan error, done chan bool) {
	ticker := time.NewTicker(10 * time.Second)

	go func() {
		time.Sleep(5 * time.Second)
		for {
			select {
			case <-done:
				ticker.Stop()
				return
			case <-ticker.C:
				conn, err := net.Dial("tcp", serverAddr)
				if err != nil {
					errCh <- err
					return
				}
				if conn != nil {
					conn.Close()
				}
			}
		}
	}()
}
