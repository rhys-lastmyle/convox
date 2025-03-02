package start

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/convox/changes"
	builder "github.com/convox/convox/pkg/build"
	"github.com/convox/convox/pkg/common"
	"github.com/convox/convox/pkg/manifest"
	"github.com/convox/convox/pkg/options"
	"github.com/convox/convox/pkg/prefix"
	"github.com/convox/convox/pkg/structs"
	"github.com/moby/buildkit/frontend/dockerfile/dockerignore"
)

const (
	ScannerStartSize = 4096
	ScannerMaxSize   = 20 * 1024 * 1024
)

var (
	reAppLog       = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})T(\d{2}:\d{2}:\d{2})Z ([^/]+)/([^/]+)/([^ ]+) (.*)$`)
	reDockerOption = regexp.MustCompile("--([a-z]+)")
)

type Options2 struct {
	App      string
	Build    bool
	Cache    bool
	External bool
	Manifest string
	Provider structs.Provider
	Services []string
	Sync     bool
	Test     bool
}

type buildSource struct {
	Local  string
	Remote string
}

func (*Start) Start2(ctx context.Context, w io.Writer, opts Options2) error {
	select {
	case <-ctx.Done():
		return nil
	default:
	}

	if opts.App == "" {
		return errors.WithStack(fmt.Errorf("app required"))
	}

	a, err := opts.Provider.AppGet(opts.App)
	if err != nil {
		if _, err := opts.Provider.AppCreate(opts.App, structs.AppCreateOptions{Generation: options.String("2")}); err != nil {
			return errors.WithStack(err)
		}
	} else {
		if a.Generation != "2" && a.Generation != "3" {
			return errors.WithStack(fmt.Errorf("invalid generation: %s", a.Generation))
		}
	}

	data, err := os.ReadFile(common.CoalesceString(opts.Manifest, "convox.yml"))
	if err != nil {
		return errors.WithStack(err)
	}

	env, err := common.AppEnvironment(opts.Provider, opts.App)
	if err != nil {
		return errors.WithStack(err)
	}

	m, err := manifest.Load(data, env)
	if err != nil {
		return errors.WithStack(err)
	}

	if err := m.Validate(); err != nil {
		return err
	}

	services := map[string]bool{}

	if opts.Services == nil {
		for i := range m.Services {
			services[m.Services[i].Name] = true
		}
	} else {
		for _, s := range opts.Services {
			services[s] = true
		}
	}

	pw := prefixWriter(w, services)

	if opts.Build {
		bopts := structs.BuildCreateOptions{
			Development: options.Bool(true),
			External:    options.Bool(opts.External),
		}

		if opts.Manifest != "" {
			bopts.Manifest = options.String(opts.Manifest)
		}

		b, err := opts.buildCreate(ctx, &pw, bopts)
		if err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return nil
		default:
		}

		popts := structs.ReleasePromoteOptions{
			Development: options.Bool(true),
			Force:       options.Bool(true),
			Idle:        options.Bool(false),
			Min:         options.Int(0),
			Timeout:     options.Int(300),
		}

		if err := opts.Provider.ReleasePromote(opts.App, b.Release, popts); err != nil {
			return errors.WithStack(err)
		}
	}

	go opts.streamLogs(ctx, pw, services)

	errch := make(chan error)
	defer close(errch)

	go handleErrors(ctx, pw, errch)

	wd, err := os.Getwd()
	if err != nil {
		return errors.WithStack(err)
	}

	for i := range m.Services {
		if !services[m.Services[i].Name] {
			continue
		}

		if m.Services[i].Build.Path != "" {
			go opts.watchChanges(ctx, pw, m, m.Services[i].Name, wd, errch)
		}
	}

	if err := common.WaitForAppRunningContext(ctx, opts.Provider, opts.App); err != nil {
		return err
	}

	<-ctx.Done()

	a, err = opts.Provider.AppGet(opts.App)
	if err != nil {
		return nil
	}

	pw.Writef("convox", "stopping\n")

	if a.Release != "" {
		popts := structs.ReleasePromoteOptions{
			Development: options.Bool(false),
			Force:       options.Bool(true),
		}

		if err := opts.Provider.ReleasePromote(opts.App, a.Release, popts); err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

func (opts Options2) buildCreate(ctx context.Context, pw *prefix.Writer, bopts structs.BuildCreateOptions) (*structs.Build, error) {
	if opts.External {
		return opts.buildCreateExternal(ctx, pw, bopts)
	}

	pw.Writef("build", "uploading source\n")

	data, err := common.Tarball(".")
	if err != nil {
		return nil, errors.WithStack(err)
	}

	o, err := opts.Provider.ObjectStore(opts.App, "", bytes.NewReader(data), structs.ObjectStoreOptions{})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	pw.Writef("build", "starting build\n")

	b, err := opts.Provider.BuildCreate(opts.App, o.Url, bopts)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	logs, err := opts.Provider.BuildLogs(opts.App, b.Id, structs.LogsOptions{})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	bo := pw.Writer("build")

	go io.Copy(bo, logs)

	if err := opts.waitForBuild(ctx, b.Id); err != nil {
		return nil, errors.WithStack(err)
	}

	b, err = opts.Provider.BuildGet(opts.App, b.Id)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return b, nil
}

func (opts Options2) buildCreateExternal(ctx context.Context, pw *prefix.Writer, bopts structs.BuildCreateOptions) (*structs.Build, error) {
	dir := "."

	s, err := opts.Provider.SystemGet()
	if err != nil {
		return nil, err
	}

	bopts.External = options.Bool(true)

	b, err := opts.Provider.BuildCreate(opts.App, "", bopts)
	if err != nil {
		return nil, err
	}

	manifest := common.CoalesceString(opts.Manifest, "convox.yml")

	data, err := os.ReadFile(filepath.Join(dir, manifest))
	if err != nil {
		return nil, err
	}

	if _, err := opts.Provider.BuildUpdate(b.App, b.Id, structs.BuildUpdateOptions{Manifest: options.String(string(data))}); err != nil {
		return nil, err
	}

	u, err := url.Parse(b.Repository)
	if err != nil {
		return nil, err
	}

	auth := ""
	repo := fmt.Sprintf("%s%s", u.Host, u.Path)

	if pass, ok := u.User.Password(); ok {
		auth = fmt.Sprintf(`{%q: { "Username": %q, "Password": %q } }`, repo, u.User.Username(), pass)
	}

	bbopts := builder.Options{
		App:         b.App,
		Auth:        auth,
		Cache:       opts.Cache,
		Development: true,
		Id:          b.Id,
		Manifest:    manifest,
		Push:        repo,
		Rack:        s.Name,
		Source:      fmt.Sprintf("dir://%s", dir),
		Terminal:    true,
	}

	bb, err := builder.New(opts.Provider, bbopts, &builder.Docker{})
	if err != nil {
		return nil, err
	}

	if err := bb.Execute(); err != nil {
		return nil, err
	}

	ropts := structs.ReleaseCreateOptions{
		Build:       options.String(b.Id),
		Description: options.String(b.Description),
	}

	r, err := opts.Provider.ReleaseCreate(b.App, ropts)
	if err != nil {
		return nil, err
	}

	uopts := structs.BuildUpdateOptions{
		Release: options.String(r.Id),
	}

	bu, err := opts.Provider.BuildUpdate(b.App, b.Id, uopts)
	if err != nil {
		return nil, err
	}

	return bu, nil
}

func (opts Options2) handleAdds(pid, remote string, adds []changes.Change) error {
	if len(adds) == 0 {
		return nil
	}

	if !filepath.IsAbs(remote) {
		var buf bytes.Buffer

		if _, err := opts.Provider.ProcessExec(opts.App, pid, "pwd", &buf, structs.ProcessExecOptions{}); err != nil {
			return errors.WithStack(fmt.Errorf("%s pwd: %s", pid, err))
		}

		wd := strings.TrimSpace(buf.String())

		remote = filepath.Join(wd, remote)
	}

	rp, wp := io.Pipe()

	ch := make(chan error)

	go func() {
		ch <- opts.Provider.FilesUpload(opts.App, pid, rp, structs.FileTransterOptions{})
		close(ch)
	}()

	tw := tar.NewWriter(wp)

	for _, add := range adds {
		local := filepath.Join(add.Base, add.Path)

		stat, err := os.Stat(local)
		if err != nil {
			// skip transient files like '.git/.COMMIT_EDITMSG.swp'
			if os.IsNotExist(err) {
				continue
			}

			return errors.WithStack(err)
		}

		tw.WriteHeader(&tar.Header{
			Name:    filepath.Join(remote, add.Path),
			Mode:    int64(stat.Mode()),
			Size:    stat.Size(),
			ModTime: stat.ModTime(),
		})

		fd, err := os.Open(local)
		if err != nil {
			return errors.WithStack(err)
		}

		defer fd.Close() // skipcq

		if _, err := io.Copy(tw, fd); err != nil {
			return errors.WithStack(err)
		}

		fd.Close()
	}

	if err := tw.Close(); err != nil {
		return errors.WithStack(err)
	}

	if err := wp.Close(); err != nil {
		return errors.WithStack(err)
	}

	return <-ch
}

func (opts Options2) handleRemoves(pid string, removes []changes.Change) error {
	if len(removes) == 0 {
		return nil
	}

	return opts.Provider.FilesDelete(opts.App, pid, changes.Files(removes))
}

func (opts Options2) stopProcess(pid string, wg *sync.WaitGroup) {
	defer wg.Done()
	opts.Provider.ProcessStop(opts.App, pid)
}

func (opts Options2) streamLogs(ctx context.Context, pw prefix.Writer, services map[string]bool) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			logs, err := opts.Provider.AppLogs(opts.App, structs.LogsOptions{Prefix: options.Bool(true), Since: options.Duration(1 * time.Second)})
			if err == nil {
				writeLogs(ctx, pw, logs, services)
			}

			select {
			case <-ctx.Done():
				return
			default:
				time.Sleep(1 * time.Second)
			}
		}
	}
}

func (opts Options2) waitForBuild(ctx context.Context, id string) error {
	tick := time.Tick(1 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
			b, err := opts.Provider.BuildGet(opts.App, id)
			if err != nil {
				return errors.WithStack(err)
			}

			switch b.Status {
			case "created", "running":
				break
			case "complete":
				return nil
			case "failed":
				return errors.WithStack(fmt.Errorf("build failed"))
			default:
				return errors.WithStack(fmt.Errorf("unknown build status: %s", b.Status))
			}
		}
	}
}

func (opts Options2) watchChanges(ctx context.Context, pw prefix.Writer, m *manifest.Manifest, service, root string, ch chan error) {
	bss, err := buildSources(m, root, service)
	if err != nil {
		ch <- fmt.Errorf("sync error: %s", err)
		return
	}

	ignores, err := buildIgnores(root, service)
	if err != nil {
		ch <- fmt.Errorf("sync error: %s", err)
		return
	}

	for _, bs := range bss {
		go opts.watchPath(ctx, pw, service, root, bs, ignores, ch)
	}
}

func (opts Options2) watchPath(ctx context.Context, pw prefix.Writer, service, root string, bs buildSource, ignores []string, ch chan error) {
	cch := make(chan changes.Change, 1)

	abs, err := filepath.Abs(bs.Local)
	if err != nil {
		ch <- fmt.Errorf("sync error: %s", err)
		return
	}

	wd, err := os.Getwd()
	if err != nil {
		ch <- fmt.Errorf("sync error: %s", err)
		return
	}

	rel, err := filepath.Rel(wd, bs.Local)
	if err != nil {
		ch <- fmt.Errorf("sync error: %s", err)
		return
	}

	pw.Writef("convox", "starting sync from <dir>%s</dir> to <dir>%s</dir> on <service>%s</service>\n", rel, common.CoalesceString(bs.Remote, "."), service)

	go changes.Watch(abs, cch, changes.WatchOptions{
		Ignores: ignores,
	})

	tick := time.Tick(1000 * time.Millisecond)
	var chgs []changes.Change

	for {
		select {
		case <-ctx.Done():
			return
		case c := <-cch:
			chgs = append(chgs, c)
		case <-tick:
			if len(chgs) == 0 {
				continue
			}

			pss, err := opts.Provider.ProcessList(opts.App, structs.ProcessListOptions{Service: options.String(service)})
			if err != nil {
				pw.Writef("convox", "sync error: %s\n", err)
				continue
			}

			adds, removes := changes.Partition(chgs)

			for _, ps := range pss {
				switch {
				case len(adds) > 3:
					pw.Writef("convox", "sync: %d files to <dir>%s</dir> on <service>%s</service>\n", len(adds), common.CoalesceString(bs.Remote, "."), service)
				case len(adds) > 0:
					for _, a := range adds {
						pw.Writef("convox", "sync: <dir>%s</dir> to <dir>%s</dir> on <service>%s</service>\n", a.Path, common.CoalesceString(bs.Remote, "."), service)
					}
				}

				if err := opts.handleAdds(ps.Id, bs.Remote, adds); err != nil {
					pw.Writef("convox", "sync add error: %s\n", err)
				}

				switch {
				case len(removes) > 3:
					pw.Writef("convox", "remove: %d files from <dir>%s</dir> to <service>%s</service>\n", len(removes), common.CoalesceString(bs.Remote, "."), service)
				case len(removes) > 0:
					for _, r := range removes {
						pw.Writef("convox", "remove: <dir>%s</dir> from <dir>%s</dir> on <service>%s</service>\n", r.Path, common.CoalesceString(bs.Remote, "."), service)
					}
				}

				if err := opts.handleRemoves(ps.Id, removes); err != nil {
					pw.Writef("convox", "sync remove error: %s\n", err)
				}
			}

			chgs = []changes.Change{}
		}
	}
}

func buildDockerfile(m *manifest.Manifest, root, service string) ([]byte, error) {
	s, err := m.Service(service)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if s.Image != "" {
		return nil, nil
	}

	path, err := filepath.Abs(filepath.Join(root, s.Build.Path, s.Build.Manifest))
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, errors.WithStack(fmt.Errorf("no such file: %s", filepath.Join(s.Build.Path, s.Build.Manifest)))
	}

	return os.ReadFile(path)
}

func buildIgnores(root, service string) ([]string, error) {
	fd, err := os.Open(filepath.Join(root, ".dockerignore"))
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return dockerignore.ReadAll(fd)
}

func buildSources(m *manifest.Manifest, root, service string) ([]buildSource, error) {
	data, err := buildDockerfile(m, root, service)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	if data == nil {
		return []buildSource{}, nil
	}

	svc, err := m.Service(service)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	var bs []buildSource
	env := map[string]string{}
	wd := ""

	s := bufio.NewScanner(bytes.NewReader(data))

lines:
	for s.Scan() {
		parts := strings.Fields(s.Text())

		if len(parts) < 1 {
			continue
		}

		switch strings.ToUpper(parts[0]) {
		case "ADD", "COPY":
			for i, p := range parts {
				if m := reDockerOption.FindStringSubmatch(p); len(m) > 1 {
					switch strings.ToLower(m[1]) {
					case "from":
						continue lines
					default:
						parts = append(parts[:i], parts[i+1:]...)
					}
				}
			}

			if len(parts) > 2 {
				u, err := url.Parse(parts[1])
				if err != nil {
					return nil, errors.WithStack(err)
				}

				if strings.HasPrefix(parts[1], "--from") {
					continue
				}

				switch u.Scheme {
				case "http", "https":
					// do nothing
				default:
					local := filepath.Join(svc.Build.Path, parts[1])
					remote := replaceEnv(parts[2], env)

					if wd != "" && !filepath.IsAbs(remote) {
						remote = filepath.Join(wd, remote)
					}

					bs = append(bs, buildSource{Local: local, Remote: remote})
				}
			}
		case "ENV":
			if len(parts) > 2 {
				env[parts[1]] = parts[2]
			}
		case "FROM":
			if len(parts) > 1 {
				var ee []string

				data, err := Exec.Execute("docker", "inspect", parts[1], "--format", "{{json .Config.Env}}")
				if err != nil {
					continue
				}

				if err := json.Unmarshal(data, &ee); err != nil {
					return nil, errors.WithStack(err)
				}

				for _, e := range ee {
					parts := strings.SplitN(e, "=", 2)

					if len(parts) == 2 {
						env[parts[0]] = parts[1]
					}
				}

				data, err = Exec.Execute("docker", "inspect", parts[1], "--format", "{{.Config.WorkingDir}}")
				if err != nil {
					return nil, errors.WithStack(err)
				}

				wd = strings.TrimSpace(string(data))
			}
		case "WORKDIR":
			if len(parts) > 1 {
				wd = replaceEnv(parts[1], env)
			}
		}
	}

	for i := range bs {
		abs, err := filepath.Abs(bs[i].Local)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		stat, err := os.Stat(abs)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		if stat.IsDir() && !strings.HasSuffix(abs, "/") {
			abs = abs + "/"
		}

		bs[i].Local = abs

		if bs[i].Remote == "." {
			bs[i].Remote = wd
		}
	}

	var bss []buildSource

	for i := range bs {
		contained := false

		for j := i + 1; j < len(bs); j++ {
			if strings.HasPrefix(bs[i].Local, bs[j].Local) {
				if bs[i].Remote == bs[j].Remote {
					contained = true
					break
				}

				rl, err := filepath.Rel(bs[j].Local, bs[i].Local)
				if err != nil {
					return nil, errors.WithStack(err)
				}

				rr, err := filepath.Rel(bs[j].Remote, bs[i].Remote)
				if err != nil {
					return nil, errors.WithStack(err)
				}

				if rl == rr {
					contained = true
					break
				}
			}
		}

		if !contained {
			bss = append(bss, bs[i])
		}
	}

	return bss, nil
}

type stackTracer interface {
	StackTrace() errors.StackTrace
}

func handleErrors(ctx context.Context, pw prefix.Writer, errch chan error) {
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-errch:
			if err != nil {
				pw.Writef("convox", "<error>error: %s</error>\n", err)
			}
		}
	}
}

func replaceEnv(s string, env map[string]string) string {
	for k, v := range env {
		s = strings.Replace(s, fmt.Sprintf("${%s}", k), v, -1)
		s = strings.Replace(s, fmt.Sprintf("$%s", k), v, -1)
	}

	return s
}

var ansiScreenSequences = []*regexp.Regexp{
	regexp.MustCompile("\033\\[\\d+;\\d+H"),
}

func stripANSIScreenCommands(data string) string {
	for _, r := range ansiScreenSequences {
		data = r.ReplaceAllString(data, "")
	}

	return data
}

func writeLogs(ctx context.Context, pw prefix.Writer, r io.Reader, services map[string]bool) {
	ls := bufio.NewScanner(r)

	ls.Buffer(make([]byte, ScannerStartSize), ScannerMaxSize)

	for ls.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
			match := reAppLog.FindStringSubmatch(ls.Text())

			if len(match) != 7 {
				continue
			}

			switch match[3] {
			case "service":
				service := match[4]

				if !services[service] {
					continue
				}

				stripped := stripANSIScreenCommands(match[6])

				pw.Writef(service, "%s\n", stripped)
			case "system":
				service := strings.Split(match[5], "-")[0]

				if !services[service] {
					continue
				}

				pw.Writef(service, "%s\n", match[6])
			}
		}
	}

	if err := ls.Err(); err != nil {
		pw.Writef("convox", "scan error: %s\n", err)
	}
}
