package agent

import (
	"context"
	"strings"
	"time"

	"github.com/charliek/craze/internal/codexrpc"
)

// Codex skills discovery, async rescan, and `/skill` prompt invocation (Plan 054 § 5 M2b).

var codexSkillStartTimeout = 5 * time.Second

func loadCodexSkills(ctx context.Context, client *codexrpc.Client, cwd string, forceReload bool) ([]Skill, map[string]codexrpc.SkillMetadata, error) {
	if client == nil {
		return nil, nil, nil
	}
	var cwds []string
	if cwd != "" {
		cwds = []string{cwd}
	}
	res, err := client.SkillsList(ctx, codexrpc.SkillsListParams{
		Cwds:        cwds,
		ForceReload: forceReload,
	})
	if err != nil || res == nil {
		return nil, nil, err
	}
	var (
		skills []Skill
		byName = make(map[string]codexrpc.SkillMetadata)
	)
	for _, entry := range res.Data {
		for _, sk := range entry.Skills {
			if !sk.Enabled {
				continue
			}
			name := strings.TrimSpace(sk.Name)
			if name == "" || !SlashNameOK(name) {
				continue
			}
			key := strings.ToLower(name)
			if _, dup := byName[key]; dup {
				continue
			}
			desc := strings.TrimSpace(sk.ShortDescription)
			if desc == "" {
				desc = strings.TrimSpace(sk.Description)
			}
			sk.Name = name
			byName[key] = sk
			skills = append(skills, Skill{
				Name:        name,
				Description: sanitizeText(desc),
			})
		}
	}
	return skills, byName, nil
}

// RescanSkills implements SkillRescanner (non-blocking): signals the coalescing
// skills worker to call `skills/list{forceReload: true}` and publish any slash
// catalog delta. If called during startup before ready, sets skillsDirty so the
// refresh runs as soon as Start finishes (L10).
func (s *codexSession) RescanSkills() {
	s.triggerSkillRefresh()
}

func (s *codexSession) handleSkillsChanged() {
	s.triggerSkillRefresh()
}

func (s *codexSession) triggerSkillRefresh() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if !s.ready || s.client == nil {
		// Retain dirty bit during startup; consumed as soon as s.ready flips true (L10).
		s.skillsDirty = true
		s.mu.Unlock()
		return
	}
	if !s.skillWorker {
		s.skillWorker = true
		go s.runSkillWorker()
	}
	s.mu.Unlock()
	select {
	case s.skillCh <- struct{}{}:
	default:
	}
}

func (s *codexSession) runSkillWorker() {
	for {
		select {
		case <-s.closingCh:
			return
		case <-s.skillCh:
		}
		s.mu.Lock()
		if s.closed || s.client == nil {
			s.mu.Unlock()
			return
		}
		client := s.client
		cwd := s.cwd
		s.mu.Unlock()

		ctx, cancel := context.WithTimeout(s.lifeCtx, codexSkillStartTimeout)
		skills, byName, err := loadCodexSkills(ctx, client, cwd, true)
		cancel()
		if err != nil {
			continue
		}

		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		s.skills = skills
		s.codexSkills = byName
		if slashDelta := s.rebuildSlashLocked(); slashDelta != nil {
			s.enqueueDeltaLocked("", Event{}, &StateDelta{Slash: slashDelta})
		}
		s.mu.Unlock()
	}
}

// parseCodexSkillPrompt checks whether promptText invokes an enabled Codex
// skill via `/<name> [rest]`, respecting Craze builtin precedence (if a builtin
// owns `/<name>` in Snapshot.Slash, it is NOT rewritten into a skill input).
func (s *codexSession) parseCodexSkillPrompt(promptText string) (sk codexrpc.SkillMetadata, rest string, ok bool) {
	trimmed := strings.TrimSpace(promptText)
	if !strings.HasPrefix(trimmed, "/") || len(trimmed) < 2 {
		return codexrpc.SkillMetadata{}, "", false
	}
	body := trimmed[1:]
	var name, after string
	if idx := strings.IndexAny(body, " \t\r\n"); idx >= 0 {
		name = body[:idx]
		after = strings.TrimSpace(body[idx+1:])
	} else {
		name = body
	}
	if name == "" {
		return codexrpc.SkillMetadata{}, "", false
	}
	key := strings.ToLower(name)

	s.mu.Lock()
	defer s.mu.Unlock()
	meta, found := s.codexSkills[key]
	if !found {
		return codexrpc.SkillMetadata{}, "", false
	}
	// Craze builtins win on collision (Plan 054 § C1 / M2b): only treat as a
	// Codex skill if Snapshot.Slash maps this name to SlashSourceSkill.
	for _, item := range s.snap.Slash {
		if strings.EqualFold(item.Name, name) {
			if item.Source != SlashSourceSkill {
				return codexrpc.SkillMetadata{}, "", false
			}
			break
		}
	}
	return meta, after, true
}
