package engines

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"

	"github.com/instantoffr/relay/internal/model"
)

// recoverInterrupted cleans up after Relay restarted in the middle of a
// container swap: a renamed "<name>-old-<ts>" container is either removed
// (the new engine runs and its agent answers) or restored.
func (s *Service) recoverInterrupted(ctx context.Context) {
	cli, err := newDockerClient()
	if err != nil {
		return
	}
	defer cli.Close()
	dctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	project := composeProject(dctx, cli)
	var notes []string
	for _, engine := range []string{"nginx", "haproxy"} {
		args := filters.NewArgs(filters.Arg("label", labelEngine+"="+engine))
		if project != "" {
			args.Add("label", labelComposeProj+"="+project)
		}
		list, err := cli.ContainerList(dctx, container.ListOptions{All: true, Filters: args})
		if err != nil {
			return
		}
		var olds []container.Summary
		for _, c := range list {
			if len(c.Names) > 0 && strings.Contains(c.Names[0], "-old-") {
				olds = append(olds, c)
			}
		}
		if len(olds) == 0 {
			continue
		}
		old := olds[len(olds)-1]
		oldName := strings.TrimPrefix(old.Names[0], "/")
		origName := oldName[:strings.LastIndex(oldName, "-old-")]
		cur, _ := findEngineContainer(dctx, cli, project, engine)
		sctx, c2 := context.WithTimeout(dctx, 5*time.Second)
		st, stErr := s.agentClient(engine).Status(sctx)
		c2()
		if cur != nil && cur.State == "running" && stErr == nil && (st.Running || !st.Configured) {
			for _, o := range olds {
				cli.ContainerRemove(dctx, o.ID, container.RemoveOptions{Force: true})
			}
			if _, _, official := officialRef(cur.Image); official {
				set := s.settings(dctx)
				if engine == "nginx" {
					set.NginxImage = cur.Image
				} else {
					set.HAProxyImage = cur.Image
				}
				s.app.Store.PutSettings(dctx, model.SettingsEngines, set)
			}
			notes = append(notes, fmt.Sprintf("%s: kept %s (new container is running), removed the old one", engineName(engine), cur.Image))
			continue
		}
		if cur != nil {
			cli.ContainerRemove(dctx, cur.ID, container.RemoveOptions{Force: true})
		}
		if err := cli.ContainerRename(dctx, old.ID, origName); err != nil {
			notes = append(notes, fmt.Sprintf("%s: couldn't restore %s: %v", engineName(engine), oldName, err))
			continue
		}
		if err := cli.ContainerStart(dctx, old.ID, container.StartOptions{}); err != nil {
			notes = append(notes, fmt.Sprintf("%s: restored %s but it didn't start: %v", engineName(engine), old.Image, err))
			continue
		}
		notes = append(notes, fmt.Sprintf("%s: restored the previous container (%s)", engineName(engine), old.Image))
	}
	if len(notes) == 0 {
		return
	}
	msg := "Recovered after an interrupted upgrade — " + strings.Join(notes, "; ")
	s.log.Warn(msg)
	s.app.Activity(ctx, "engine.upgrade", "warn", "Engine upgrade recovered after restart", "", msg)
	s.mu.Lock()
	if s.job != nil {
		s.job.Message = msg
	}
	j := s.job
	s.mu.Unlock()
	if j != nil {
		if b, err := json.Marshal(j); err == nil {
			s.app.Store.PutKV(context.WithoutCancel(ctx), kvLastJob, b)
		}
	}
}
