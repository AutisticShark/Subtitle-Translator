<script lang="ts">
  import { onDestroy } from 'svelte';
  import SettingsDialog from './SettingsDialog.svelte';
  import { api, errorMessage, isAborted, json } from '../lib/api';
  import { appState, notify, t } from '../lib/appState.svelte';
  import type { User } from '../lib/types';
  import { activeJobCount, loadUsers } from '../lib/workspace.svelte';

  let settingsDialog: SettingsDialog;
  let resetDialog: HTMLDialogElement;
  let resetTarget = $state<User | null>(null);
  let resetPassword = $state('');
  let resetBusy = $state(false);
  let resetError = $state('');
  let newUsername = $state('');
  let newPassword = $state('');
  let newRole = $state<'user' | 'admin'>('user');

  const users = $derived(appState.users);
  const activeUsers = $derived(users.filter((user) => user.active).length);
  const admins = $derived(users.filter((user) => user.role === 'admin' && user.active).length);
  const activeJobs = $derived(activeJobCount(appState.overview.counts));
  const providers = $derived(Object.keys(appState.settings?.providers || {}));
  const readyProviders = $derived(
    providers.filter((key) => appState.settings?.configured?.[key]).length,
  );

  onDestroy(() => {
    newPassword = '';
    resetPassword = '';
    if (resetDialog?.open) resetDialog.close();
  });
  $effect(() => {
    if (appState.view !== 'admin' && resetDialog?.open) resetDialog.close();
  });

  async function createUser(event: SubmitEvent) {
    event.preventDefault();
    const generation = appState.session;
    try {
      await json('/api/users', { username: newUsername, password: newPassword, role: newRole });
      if (generation !== appState.session) return;
      newUsername = '';
      newPassword = '';
      newRole = 'user';
      await loadUsers();
      notify(t('User created'));
    } catch (cause) {
      if (generation === appState.session) notify(errorMessage(cause));
    }
  }
  async function updateUser(
    user: User,
    method: 'PATCH' | 'DELETE',
    payload: Record<string, unknown> | undefined,
  ) {
    const generation = appState.session;
    try {
      await api(`/api/users/${encodeURIComponent(user.id)}`, {
        method,
        headers: payload ? { 'Content-Type': 'application/json' } : {},
        body: payload ? JSON.stringify(payload) : undefined,
      });
      if (generation !== appState.session) return;
      await loadUsers();
      notify(t(method === 'DELETE' ? 'User deleted' : 'User updated'));
    } catch (cause) {
      if (generation !== appState.session) return;
      notify(errorMessage(cause));
      await loadUsers().catch(() => {});
    }
  }
  async function changeRole(user: User, event: Event) {
    const select = event.currentTarget as HTMLSelectElement;
    const role = select.value;
    await updateUser(user, 'PATCH', { role });
    // A rejected change leaves the saved role; show it again.
    const saved = appState.users.find((item) => item.id === user.id);
    if (saved) select.value = saved.role;
  }
  function removeUser(user: User) {
    if (
      !window.confirm(
        t('Delete user {username}? Their finished jobs will remain for administrator cleanup.', {
          username: user.username,
        }),
      )
    )
      return;
    void updateUser(user, 'DELETE', undefined);
  }
  function openReset(user: User) {
    resetTarget = user;
    resetPassword = '';
    resetError = '';
    resetDialog.showModal();
  }
  function closeReset() {
    resetPassword = '';
    resetError = '';
    resetTarget = null;
  }
  async function submitReset(event: SubmitEvent) {
    event.preventDefault();
    const user = resetTarget;
    if (!user) return;
    const generation = appState.session;
    resetBusy = true;
    resetError = '';
    try {
      await json(`/api/users/${encodeURIComponent(user.id)}`, { password: resetPassword }, 'PATCH');
      if (generation !== appState.session) return;
      resetDialog.close();
      await loadUsers();
      notify(t('User updated'));
    } catch (cause) {
      if (generation === appState.session && !isAborted(cause)) resetError = errorMessage(cause);
    } finally {
      resetPassword = '';
      resetBusy = false;
    }
  }
</script>

<div class="admin-hero">
  <div>
    <span class="eyebrow">{t('Administration')}</span>
    <h2>{t('Panel control center')}</h2>
    <p>{t('Manage access, provider readiness, and panel-wide translation activity.')}</p>
  </div>
  <button class="primary" id="settingsButton" type="button" onclick={() => settingsDialog.open()}
    >{t('Configure panel')} <span>→</span></button
  >
</div>
<div class="metric-grid admin-metrics" id="adminMetrics">
  <article class="metric-card">
    <span class="metric-label">{t('Total users')}</span>
    <strong class="metric-value">{users.length}</strong>
    <span class="metric-note">{t('{count} active accounts', { count: activeUsers })}</span>
  </article>
  <article class="metric-card">
    <span class="metric-label">{t('Active administrators')}</span>
    <strong class="metric-value">{admins}</strong>
    <span class="metric-note">{t('Protected panel access')}</span>
  </article>
  <article class="metric-card" class:accent={activeJobs > 0}>
    <span class="metric-label">{t('Panel jobs')}</span>
    <strong class="metric-value">{appState.overview.total}</strong>
    <span class="metric-note">{t('{count} currently active', { count: activeJobs })}</span>
  </article>
  <article class="metric-card">
    <span class="metric-label">{t('Ready providers')}</span>
    <strong class="metric-value">{readyProviders}/{providers.length}</strong>
    <span class="metric-note">{t('Configured for translation')}</span>
  </article>
</div>
<div class="admin-grid">
  <section class="panel dashboard-card admin-providers-card">
    <div class="card-heading">
      <div>
        <span class="eyebrow">{t('Providers')}</span>
        <h2>{t('Provider readiness')}</h2>
      </div>
      <button
        class="text-button"
        id="providerSettingsButton"
        type="button"
        onclick={() => settingsDialog.open()}>{t('Edit settings')}</button
      >
    </div>
    <div class="provider-admin-list" id="adminProviders">
      {#each Object.entries(appState.settings?.providers || {}) as [key, label] (key)}
        {@const ready = appState.settings?.configured?.[key]}
        <div class="provider-admin-row">
          <div>
            <strong>{label}</strong><small
              >{key === appState.settings?.default_provider
                ? t('Panel default')
                : t('Available provider')}</small
            >
          </div>
          <span class="readiness" class:ready>{t(ready ? 'Configured' : 'Not set')}</span>
        </div>
      {/each}
    </div>
  </section>
  <section class="panel dashboard-card user-management-card">
    <div class="card-heading">
      <div>
        <span class="eyebrow">{t('Access')}</span>
        <h2>{t('User management')}</h2>
      </div>
    </div>
    <form id="createUserForm" class="create-user-form" onsubmit={createUser}>
      <label
        >{t('Username')}<input
          name="username"
          bind:value={newUsername}
          required
          minlength="3"
          maxlength="64"
          autocomplete="off"
        /></label
      >
      <label
        >{t('Temporary password')}<input
          name="password"
          type="password"
          bind:value={newPassword}
          required
          minlength="12"
          maxlength="256"
          autocomplete="new-password"
        /></label
      >
      <label
        >{t('Role')}<select name="role" bind:value={newRole}
          ><option value="user">{t('User')}</option><option value="admin"
            >{t('Administrator')}</option
          ></select
        ></label
      >
      <button class="primary" type="submit">{t('Create user')}</button>
    </form>
    <div class="user-list" id="userList">
      {#each users as user (user.id)}
        {@const self = user.id === appState.user?.id}
        <article class="user-row" data-user-id={user.id}>
          <div>
            <strong>{user.username}</strong>
            <div class="job-meta">
              {t('{count} jobs', { count: user.job_count ?? 0 })} · {t(
                user.active ? 'active' : 'disabled',
              )}{#if user.locked}
                · {t('locked')}{/if}
            </div>
          </div>
          <select
            class="user-role"
            value={user.role}
            disabled={self}
            aria-label={t('Role for {username}', { username: user.username })}
            onchange={(event) => changeRole(user, event)}
          >
            <option value="user">{t('User')}</option>
            <option value="admin">{t('Administrator')}</option>
          </select>
          <div class="user-actions">
            {#if user.locked}<button
                class="unlock-user ghost small"
                type="button"
                onclick={() => updateUser(user, 'PATCH', { unlock: true })}>{t('Unlock')}</button
              >{/if}
            {#if !self}
              <button class="reset-user ghost small" type="button" onclick={() => openReset(user)}
                >{t('Reset password')}</button
              ><button
                class="toggle-user ghost small"
                type="button"
                onclick={() => updateUser(user, 'PATCH', { active: !user.active })}
                >{t(user.active ? 'Disable' : 'Enable')}</button
              ><button
                class="remove-user ghost small"
                type="button"
                onclick={() => removeUser(user)}>{t('Delete')}</button
              >
            {/if}
          </div>
        </article>
      {/each}
    </div>
  </section>
</div>
<SettingsDialog bind:this={settingsDialog} />
<dialog id="resetPasswordDialog" bind:this={resetDialog} onclose={closeReset}>
  <form onsubmit={submitReset}>
    <div class="dialog-heading">
      <h2>{t('Reset password')}</h2>
      <button
        class="icon-button"
        type="button"
        aria-label={t('Close')}
        onclick={() => resetDialog.close()}>×</button
      >
    </div>
    <div class="settings-group">
      <label
        >{t('New password for {username} (12+ characters):', {
          username: resetTarget?.username || '',
        })}<input
          type="password"
          bind:value={resetPassword}
          required
          minlength="12"
          maxlength="256"
          autocomplete="new-password"
        /></label
      >
      <div class="auth-error" role="alert">{resetError}</div>
    </div>
    <div class="dialog-actions">
      <button class="ghost" type="button" onclick={() => resetDialog.close()}>{t('Cancel')}</button
      ><button class="primary" type="submit" disabled={resetBusy}>{t('Reset password')}</button>
    </div>
  </form>
</dialog>
