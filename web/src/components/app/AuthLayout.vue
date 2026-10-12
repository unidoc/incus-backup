<script setup lang="ts">
// Centered card for sign-in, setup and forced password change.
import Logo from './Logo.vue'
import { instanceHost } from '@/lib/host'
defineProps<{ title: string; lede?: string; wide?: boolean }>()
</script>

<template>
  <div class="auth">
    <div class="side" aria-hidden="true">
      <div class="brand"><Logo :size="28" /><span>incus-backup</span></div>
      <p class="claim">Encrypted, deduplicated backups for Incus. The server never holds a key.</p>
      <ul class="facts">
        <li><b>Zero knowledge</b>Snapshots are encrypted on the host before they leave it.</li>
        <li><b>Verified</b>Every chunk is checked; corrupt data is quarantined and healed.</li>
        <li><b>One control plane</b>PostgreSQL holds the state; the datastore is the truth.</li>
      </ul>
      <span class="host">{{ instanceHost() }}</span>
    </div>
    <main class="pane">
      <div class="card" :class="{ wide }">
        <div class="brand-m"><Logo :size="24" /><span>incus-backup</span></div>
        <h1>{{ title }}</h1>
        <p v-if="lede" class="lede">{{ lede }}</p>
        <slot />
      </div>
      <p class="foot"><slot name="foot" /></p>
    </main>
  </div>
</template>

<style scoped>
.auth { display: grid; grid-template-columns: minmax(320px, 440px) 1fr; min-height: 100dvh; }
.side { display: flex; flex-direction: column; gap: 28px; padding: 40px 40px 28px; background: var(--nav-bg); color: var(--nav-text); }
.brand, .brand-m { display: flex; align-items: center; gap: 10px; font-weight: var(--fw-semibold); font-size: 16px; color: var(--nav-text-active); }
.claim { font-size: 22px; line-height: 1.3; color: var(--nav-text-active); font-weight: var(--fw-medium); letter-spacing: -0.01em; margin-top: 12vh; max-width: 28ch; }
.facts { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 16px; font-size: var(--fs-sm); color: var(--nav-text-muted); max-width: 34ch; }
.facts b { display: block; color: var(--nav-text); font-weight: var(--fw-semibold); margin-bottom: 2px; }
.facts li { padding-left: 14px; border-left: 2px solid var(--nav-marker); }
.host { margin-top: auto; font-family: var(--font-mono); font-size: var(--fs-xs); color: var(--nav-text-muted); }
.pane { display: flex; flex-direction: column; align-items: center; justify-content: center; padding: 32px 16px; gap: 16px; }
.card { width: 100%; max-width: 380px; display: flex; flex-direction: column; gap: 16px; }
.card.wide { max-width: 440px; }
.brand-m { display: none; color: var(--text); }
h1 { font-size: 22px; letter-spacing: -0.01em; }
.lede { color: var(--text-muted); margin-top: -8px; }
.foot { font-size: var(--fs-xs); color: var(--text-muted); text-align: center; max-width: 380px; }
@media (max-width: 860px) {
  .auth { grid-template-columns: 1fr; }
  .side { display: none; }
  .brand-m { display: flex; }
  .pane { justify-content: flex-start; padding-top: 12vh; }
}
</style>
