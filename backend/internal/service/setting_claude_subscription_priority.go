package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const SettingKeyClaudeSubscriptionPriorityGroupIDs = "claude_subscription_priority_group_ids"

const claudeSubscriptionPriorityCacheTTL = 5 * time.Second
const claudeSubscriptionPriorityDBTimeout = 3 * time.Second

var ErrClaudeSubscriptionPriorityGroupInvalid = infraerrors.BadRequest(
	"CLAUDE_SUBSCRIPTION_PRIORITY_GROUP_INVALID",
	"subscription priority groups must be active Anthropic or Composite groups with positive IDs",
)

type ClaudeSubscriptionPrioritySettings struct {
	Supported                bool     `json:"supported"`
	Strategy                 string   `json:"strategy"`
	EnabledGroupIDs          []int64  `json:"enabled_group_ids"`
	SubscriptionAccountTypes []string `json:"subscription_account_types"`
}

type cachedClaudeSubscriptionPriority struct {
	groupIDs  []int64
	enabled   map[int64]struct{}
	err       error
	expiresAt time.Time
}

func normalizeClaudeSubscriptionPriorityGroupIDs(groupIDs []int64) ([]int64, error) {
	unique := make(map[int64]struct{}, len(groupIDs))
	for _, id := range groupIDs {
		if id <= 0 {
			return nil, ErrClaudeSubscriptionPriorityGroupInvalid
		}
		unique[id] = struct{}{}
	}
	result := make([]int64, 0, len(unique))
	for id := range unique {
		result = append(result, id)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}

func newClaudeSubscriptionPriorityCache(groupIDs []int64, err error) *cachedClaudeSubscriptionPriority {
	entry := &cachedClaudeSubscriptionPriority{
		groupIDs: append([]int64{}, groupIDs...), enabled: make(map[int64]struct{}, len(groupIDs)),
		err: err, expiresAt: time.Now().Add(claudeSubscriptionPriorityCacheTTL),
	}
	if err == nil {
		for _, id := range groupIDs {
			entry.enabled[id] = struct{}{}
		}
	}
	return entry
}

func claudeSubscriptionPrioritySettings(entry *cachedClaudeSubscriptionPriority) *ClaudeSubscriptionPrioritySettings {
	return &ClaudeSubscriptionPrioritySettings{
		Supported: true, Strategy: "subscription_first", EnabledGroupIDs: append([]int64{}, entry.groupIDs...),
		SubscriptionAccountTypes: []string{AccountTypeOAuth, AccountTypeSetupToken},
	}
}

func (s *SettingService) loadClaudeSubscriptionPriority(ctx context.Context) *cachedClaudeSubscriptionPriority {
	if s == nil || s.settingRepo == nil {
		return newClaudeSubscriptionPriorityCache(nil, fmt.Errorf("claude subscription priority settings are unavailable"))
	}
	if cached, ok := s.claudeSubscriptionPriorityCache.Load().(*cachedClaudeSubscriptionPriority); ok && cached != nil && time.Now().Before(cached.expiresAt) {
		return cached
	}
	// Readers and writes share this lock: a slow cold read cannot publish an old
	// configuration after a successful update. Hot reads only load immutable data.
	s.claudeSubscriptionPriorityMu.Lock()
	defer s.claudeSubscriptionPriorityMu.Unlock()
	if cached, ok := s.claudeSubscriptionPriorityCache.Load().(*cachedClaudeSubscriptionPriority); ok && cached != nil && time.Now().Before(cached.expiresAt) {
		return cached
	}
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claudeSubscriptionPriorityDBTimeout)
	defer cancel()
	raw, err := s.settingRepo.GetValue(dbCtx, SettingKeyClaudeSubscriptionPriorityGroupIDs)
	var ids []int64
	if errors.Is(err, ErrSettingNotFound) {
		err = nil
	} else if err == nil {
		// Missing keys default to disabled; malformed or null stored values are
		// operational errors and must never be presented as verified disabled.
		if !strings.HasPrefix(strings.TrimSpace(raw), "[") {
			err = fmt.Errorf("claude subscription priority setting must be a JSON array")
		} else if parseErr := json.Unmarshal([]byte(raw), &ids); parseErr != nil {
			err = fmt.Errorf("decode Claude subscription priority groups: %w", parseErr)
		}
	}
	if err == nil {
		var validationErr error
		ids, validationErr = normalizeClaudeSubscriptionPriorityGroupIDs(ids)
		if validationErr != nil {
			err = fmt.Errorf("stored Claude subscription priority groups contain invalid IDs")
		}
	}
	if err != nil {
		ids = nil
	}
	entry := newClaudeSubscriptionPriorityCache(ids, err)
	s.claudeSubscriptionPriorityCache.Store(entry)
	return entry
}

func (s *SettingService) GetClaudeSubscriptionPriority(ctx context.Context) (*ClaudeSubscriptionPrioritySettings, error) {
	entry := s.loadClaudeSubscriptionPriority(ctx)
	if entry.err != nil {
		return nil, entry.err
	}
	return claudeSubscriptionPrioritySettings(entry), nil
}

// IsClaudeSubscriptionPriorityEnabled is fail-closed on missing or unreadable
// configuration. Expired enabled values are never used after a failed refresh.
func (s *SettingService) IsClaudeSubscriptionPriorityEnabled(ctx context.Context, groupID int64) bool {
	if groupID <= 0 {
		return false
	}
	entry := s.loadClaudeSubscriptionPriority(ctx)
	_, enabled := entry.enabled[groupID]
	return entry.err == nil && enabled
}

func (s *SettingService) UpdateClaudeSubscriptionPriority(ctx context.Context, groupIDs []int64) (*ClaudeSubscriptionPrioritySettings, error) {
	if s == nil || s.settingRepo == nil {
		return nil, fmt.Errorf("claude subscription priority settings are unavailable")
	}
	ids, err := normalizeClaudeSubscriptionPriorityGroupIDs(groupIDs)
	if err != nil {
		return nil, err
	}
	if len(ids) > 0 && s.defaultSubGroupReader == nil {
		return nil, fmt.Errorf("group reader is required to enable Claude subscription priority")
	}
	for _, id := range ids {
		group, err := s.defaultSubGroupReader.GetByID(ctx, id)
		if err != nil && !errors.Is(err, ErrGroupNotFound) {
			return nil, fmt.Errorf("read subscription priority group %d: %w", id, err)
		}
		if group == nil || group.ID != id || group.Status != StatusActive || (group.Platform != PlatformAnthropic && group.Platform != PlatformComposite) {
			return nil, ErrClaudeSubscriptionPriorityGroupInvalid.WithMetadata(map[string]string{"group_id": strconv.FormatInt(id, 10)})
		}
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	s.claudeSubscriptionPriorityMu.Lock()
	defer s.claudeSubscriptionPriorityMu.Unlock()
	if err := s.settingRepo.Set(ctx, SettingKeyClaudeSubscriptionPriorityGroupIDs, string(encoded)); err != nil {
		// Do not preserve a possibly stale enabled value after an uncertain write.
		s.claudeSubscriptionPriorityCache.Store((*cachedClaudeSubscriptionPriority)(nil))
		return nil, err
	}
	entry := newClaudeSubscriptionPriorityCache(ids, nil)
	s.claudeSubscriptionPriorityCache.Store(entry)
	return claudeSubscriptionPrioritySettings(entry), nil
}
