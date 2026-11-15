<script lang="ts">
  import { onMount } from 'svelte';
  import Icon from './Icon.svelte';
  import { api, errorMessage, isAborted, json, sessionApi } from '../lib/api';
  import { notify, appState, t } from '../lib/appState.svelte';
  import type { User } from '../lib/types';

  let users = $state<User[]>([]);
  let loading = $state(true);
  let busy = $state(false);
  let error = $state('');
  let dialogError = $state('');
  let editor = $state<'create' | 'edit' | null>(null);
  let selected = $state<User | null>(null);
  let username = $state('');
  let password = $state('');
  let role = $state<'admin' | 'user'>('user');
  let active = $state(true);
  let confirmingDelete = $state(false);
  let dialog: HTMLDialogElement;

  async function loadUsers() {
    loading = true;
    try {
      users = (await sessionApi<{ users: User[] }>('/api/users')).users;
      error = '';
    } catch (cause) {
      if (!isAborted(cause)) error = errorMessage(cause);
    } finally {
      loading = false;
    }
  }
  onMount(() => {
    void loadUsers();
  });
  function open(user: User | null) {
    selected = user;
    editor = user ? 'edit' : 'create';
    username = user?.username || '';
    role = user?.role || 'user';
    active = user?.active ?? true;
    password = '';
    dialogError = '';
    confirmingDelete = false;
    dialog.showModal();
  }
  function close() {
    editor = null;
    selected = null;
    password = '';
    dialogError = '';
    confirmingDelete = false;
  }
  async function save(event: SubmitEvent) {
    event.preventDefault();
    busy = true;
    dialogError = '';
    const generation = appState.session;
    try {
      if (editor === 'create') await json('/api/users', { username, password, role });
      else if (selected) {
        const payload: Record<string, unknown> = {};
        if (selected.role !== role) payload.role = role;
        if (selected.active !== active) payload.active = active;
        if (password) payload.password = password;
        await json(`/api/users/${encodeURIComponent(selected.id)}`, payload, 'PATCH');
      }
      if (generation !== appState.session) return;
      notify(t(editor === 'create' ? 'User created' : 'User updated'));
      dialog.close();
      await loadUsers();
    } catch (cause) {
      if (generation === appState.session) dialogError = errorMessage(cause);
    } finally {
      password = '';
      if (generation === appState.session) busy = false;
    }
  }
  async function unlock(user: User) {
    busy = true;
    try {
      await json(`/api/users/${encodeURIComponent(user.id)}`, { unlock: true }, 'PATCH');
      await loadUsers();
      notify(t('User updated'));
    } catch (cause) {
      error = errorMessage(cause);
    } finally {
      busy = false;
    }
  }
  async function deleteUser() {
    if (!selected) return;
    busy = true;
    dialogError = '';
    try {
      await api(`/api/users/${encodeURIComponent(selected.id)}`, { method: 'DELETE' });
      dialog.close();
      await loadUsers();
      notify(t('User deleted'));
    } catch (cause) {
      dialogError = errorMessage(cause);
    } finally {
      busy = false;
    }
  }
</script>

<header class="page-header">
  <h1>{t('Users')}</h1>
  <button class="primary" onclick={() => open(null)}
    ><Icon name="plus" size={17} />{t('Create user')}</button
  >
</header>
{#if error}<div class="inline-error" role="alert">
    <span>{error}</span><button class="text-button" onclick={loadUsers}>{t('Try again')}</button>
  </div>{/if}
<div class="users-panel">
  {#if loading && !users.length}<div class="empty-state">
      <span class="loading-line"></span>{t('Loading…')}
    </div>
  {:else}<table class="users-table">
      <thead
        ><tr
          ><th>{t('Username')}</th><th>{t('Role')}</th><th>{t('Status')}</th><th>{t('Jobs')}</th><th
            ><span class="visually-hidden">{t('Actions')}</span></th
          ></tr
        ></thead
      ><tbody>
        {#each users as user (user.id)}<tr
            ><td
              ><div class="user-name">
                <span class="avatar">{user.username.slice(0, 1).toUpperCase()}</span><span
                  >{user.username}</span
                >{#if user.id === appState.user?.id}<span class="muted small-text">{t('You')}</span
                  >{/if}
              </div></td
            ><td>{t(user.role === 'admin' ? 'Administrator' : 'User')}</td><td
              ><span class="user-status"
                ><span
                  class="status-dot"
                  class:completed={user.active && !user.locked}
                  class:failed={user.locked}
                ></span>{t(user.locked ? 'locked' : user.active ? 'active' : 'disabled')}</span
              ></td
            ><td class="tabular">{user.job_count || 0}</td><td
              ><div class="user-actions">
                {#if user.locked}<button
                    class="text-button"
                    disabled={busy}
                    onclick={() => unlock(user)}>{t('Unlock')}</button
                  >{/if}{#if user.id !== appState.user?.id}<button
                    class="button small"
                    onclick={() => open(user)}
                    aria-label={t('Edit {username}', { username: user.username })}
                    >{t('Edit')}</button
                  >{/if}
              </div></td
            ></tr
          >{/each}
      </tbody>
    </table>{/if}
</div>
<dialog bind:this={dialog!} onclose={close} class="user-dialog">
  {#if editor}
    <div class="dialog-heading">
      <h2>
        {confirmingDelete ? t('Delete user?') : editor === 'create' ? t('Create user') : username}
      </h2>
      <button
        class="icon-button"
        aria-label={t('Close')}
        disabled={busy}
        onclick={() => dialog.close()}><Icon name="close" /></button
      >
    </div>
    {#if confirmingDelete}
      <p class="muted">
        {t('Delete user {username}? Their finished jobs will remain for administrator cleanup.', {
          username,
        })}
      </p>
      {#if dialogError}<p class="form-error" role="alert">{dialogError}</p>{/if}
      <div class="dialog-actions">
        <button
          class="button"
          disabled={busy}
          onclick={() => {
            confirmingDelete = false;
          }}>{t('Cancel')}</button
        ><button class="button danger" disabled={busy} onclick={deleteUser}
          >{t('Delete user')}</button
        >
      </div>
    {:else}
      <form class="stack" onsubmit={save}>
        {#if editor === 'create'}<label
            >{t('Username')}<input
              bind:value={username}
              required
              minlength="3"
              maxlength="64"
              autocomplete="off"
              autocapitalize="none"
              spellcheck="false"
            /></label
          >{/if}
        <label
          >{t(editor === 'create' ? 'Password' : 'New password')}<input
            type="password"
            bind:value={password}
            required={editor === 'create'}
            minlength="12"
            maxlength="256"
            autocomplete="new-password"
            placeholder={editor === 'edit' ? t('Leave blank to keep current password') : ''}
          /></label
        >
        <label
          >{t('Role')}<select bind:value={role}
            ><option value="user">{t('User')}</option><option value="admin"
              >{t('Administrator')}</option
            ></select
          ></label
        >
        {#if editor === 'edit'}<label class="inline-checkbox"
            ><input type="checkbox" bind:checked={active} />{t('Account enabled')}</label
          >{/if}
        {#if dialogError}<p class="form-error" role="alert">{dialogError}</p>{/if}
        <div class="dialog-actions spaced">
          {#if editor === 'edit'}<button
              type="button"
              class="text-button danger-text"
              disabled={busy}
              onclick={() => {
                confirmingDelete = true;
                dialogError = '';
              }}>{t('Delete user')}</button
            >{:else}<span></span>{/if}<button type="submit" class="primary" disabled={busy}
            >{busy ? t('Saving…') : t(editor === 'create' ? 'Create user' : 'Save changes')}</button
          >
        </div>
      </form>
    {/if}
  {/if}
</dialog>
