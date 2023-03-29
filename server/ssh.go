package server

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/log"
	"github.com/charmbracelet/soft-serve/server/backend"
	cm "github.com/charmbracelet/soft-serve/server/cmd"
	"github.com/charmbracelet/soft-serve/server/config"
	"github.com/charmbracelet/soft-serve/server/hooks"
	"github.com/charmbracelet/soft-serve/server/utils"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	bm "github.com/charmbracelet/wish/bubbletea"
	lm "github.com/charmbracelet/wish/logging"
	rm "github.com/charmbracelet/wish/recover"
	"github.com/muesli/termenv"
	gossh "golang.org/x/crypto/ssh"
)

// SSHServer is a SSH server that implements the git protocol.
type SSHServer struct {
	srv *ssh.Server
	cfg *config.Config
}

// NewSSHServer returns a new SSHServer.
func NewSSHServer(cfg *config.Config, hooks hooks.Hooks) (*SSHServer, error) {
	var err error
	s := &SSHServer{cfg: cfg}
	logger := logger.StandardLog(log.StandardLogOptions{ForceLevel: log.DebugLevel})
	mw := []wish.Middleware{
		rm.MiddlewareWithLogger(
			logger,
			// BubbleTea middleware.
			bm.MiddlewareWithProgramHandler(SessionHandler(cfg), termenv.ANSI256),
			// CLI middleware.
			cm.Middleware(cfg, hooks),
			// Git middleware.
			s.Middleware(cfg),
			// Logging middleware.
			lm.MiddlewareWithLogger(logger),
		),
	}
	s.srv, err = wish.NewServer(
		ssh.PublicKeyAuth(s.PublicKeyHandler),
		ssh.KeyboardInteractiveAuth(s.KeyboardInteractiveHandler),
		wish.WithAddress(cfg.SSH.ListenAddr),
		wish.WithHostKeyPath(filepath.Join(cfg.DataPath, cfg.SSH.KeyPath)),
		wish.WithMiddleware(mw...),
	)
	if err != nil {
		return nil, err
	}

	if cfg.SSH.MaxTimeout > 0 {
		s.srv.MaxTimeout = time.Duration(cfg.SSH.MaxTimeout) * time.Second
	}
	if cfg.SSH.IdleTimeout > 0 {
		s.srv.IdleTimeout = time.Duration(cfg.SSH.IdleTimeout) * time.Second
	}

	return s, nil
}

// ListenAndServe starts the SSH server.
func (s *SSHServer) ListenAndServe() error {
	return s.srv.ListenAndServe()
}

// Serve starts the SSH server on the given net.Listener.
func (s *SSHServer) Serve(l net.Listener) error {
	return s.srv.Serve(l)
}

// Close closes the SSH server.
func (s *SSHServer) Close() error {
	return s.srv.Close()
}

// Shutdown gracefully shuts down the SSH server.
func (s *SSHServer) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

// PublicKeyAuthHandler handles public key authentication.
func (s *SSHServer) PublicKeyHandler(ctx ssh.Context, pk ssh.PublicKey) bool {
	return s.cfg.Backend.AccessLevel("", pk) >= backend.ReadOnlyAccess
}

// KeyboardInteractiveHandler handles keyboard interactive authentication.
func (s *SSHServer) KeyboardInteractiveHandler(ctx ssh.Context, _ gossh.KeyboardInteractiveChallenge) bool {
	return s.cfg.Backend.AllowKeyless() && s.PublicKeyHandler(ctx, nil)
}

// Middleware adds Git server functionality to the ssh.Server. Repos are stored
// in the specified repo directory. The provided Hooks implementation will be
// checked for access on a per repo basis for a ssh.Session public key.
// Hooks.Push and Hooks.Fetch will be called on successful completion of
// their commands.
func (s *SSHServer) Middleware(cfg *config.Config) wish.Middleware {
	return func(sh ssh.Handler) ssh.Handler {
		return func(s ssh.Session) {
			func() {
				cmd := s.Command()
				if len(cmd) >= 2 && strings.HasPrefix(cmd[0], "git") {
					gc := cmd[0]
					// repo should be in the form of "repo.git"
					name := utils.SanitizeRepo(cmd[1])
					pk := s.PublicKey()
					access := cfg.Backend.AccessLevel(name, pk)
					// git bare repositories should end in ".git"
					// https://git-scm.com/docs/gitrepository-layout
					repo := name + ".git"
					reposDir := filepath.Join(cfg.DataPath, "repos")
					if err := ensureWithin(reposDir, repo); err != nil {
						sshFatal(s, err)
						return
					}

					repoDir := filepath.Join(reposDir, repo)
					switch gc {
					case receivePackBin:
						if access < backend.ReadWriteAccess {
							sshFatal(s, ErrNotAuthed)
							return
						}
						if _, err := cfg.Backend.Repository(name); err != nil {
							if _, err := cfg.Backend.CreateRepository(name, false); err != nil {
								log.Errorf("failed to create repo: %s", err)
								sshFatal(s, err)
								return
							}
						}
						if err := receivePack(s, s, s.Stderr(), repoDir); err != nil {
							sshFatal(s, ErrSystemMalfunction)
						}
						return
					case uploadPackBin, uploadArchiveBin:
						if access < backend.ReadOnlyAccess {
							sshFatal(s, ErrNotAuthed)
							return
						}
						gitPack := uploadPack
						if gc == uploadArchiveBin {
							gitPack = uploadArchive
						}
						err := gitPack(s, s, s.Stderr(), repoDir)
						if errors.Is(err, ErrInvalidRepo) {
							sshFatal(s, ErrInvalidRepo)
						} else if err != nil {
							sshFatal(s, ErrSystemMalfunction)
						}
					}
				}
			}()
			sh(s)
		}
	}
}

// sshFatal prints to the session's STDOUT as a git response and exit 1.
func sshFatal(s ssh.Session, v ...interface{}) {
	writePktline(s, v...)
	s.Exit(1) // nolint: errcheck
}
