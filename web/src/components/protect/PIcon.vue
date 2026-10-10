<script setup lang="ts">
// Icons of the "Protection & recovery" pages that the shared set does not have, plus every shared one
// (so a page can use one component for both). Same look as components/icons/Icon.vue.
import { computed } from 'vue'
import { icons as shared, type IconName } from '@/components/icons'
import iLink from 'lucide-static/icons/link.svg?raw'
import iFileText from 'lucide-static/icons/file-text.svg?raw'
import iImage from 'lucide-static/icons/image.svg?raw'
import iFileArchive from 'lucide-static/icons/file-archive.svg?raw'
import iFolderOpen from 'lucide-static/icons/folder-open.svg?raw'
import iKey from 'lucide-static/icons/key.svg?raw'
import iListTree from 'lucide-static/icons/list-tree.svg?raw'
import iArrowLeft from 'lucide-static/icons/arrow-left.svg?raw'
import iUpload from 'lucide-static/icons/upload.svg?raw'
import iFileSearch from 'lucide-static/icons/file-search.svg?raw'
import iHardDriveDownload from 'lucide-static/icons/hard-drive-download.svg?raw'
import iShieldX from 'lucide-static/icons/shield-x.svg?raw'
import iShieldQuestion from 'lucide-static/icons/shield-question.svg?raw'
import iTimer from 'lucide-static/icons/timer.svg?raw'
import iRotateCcw from 'lucide-static/icons/rotate-ccw.svg?raw'
import iFileLock from 'lucide-static/icons/file-lock.svg?raw'
import iPackageOpen from 'lucide-static/icons/package-open.svg?raw'
import iBan from 'lucide-static/icons/ban.svg?raw'

const rawExtra = {
  link: iLink, 'file-text': iFileText, image: iImage, 'file-archive': iFileArchive, 'folder-open': iFolderOpen, key: iKey, 'list-tree': iListTree,
  'arrow-left': iArrowLeft, upload: iUpload, 'file-search': iFileSearch, 'hard-drive-download': iHardDriveDownload, 'shield-x': iShieldX,
  'shield-question': iShieldQuestion, timer: iTimer, 'rotate-ccw': iRotateCcw, 'file-lock': iFileLock, 'package-open': iPackageOpen, ban: iBan,
} as const
const strip = (v: string) => v.replace(/<!--[\s\S]*?-->/g, '').replace(/^[\s\S]*?<svg[^>]*>/, '').replace(/<\/svg>\s*$/, '').trim()
const extra: Record<string, string> = Object.fromEntries(Object.entries(rawExtra).map(([k, v]) => [k, strip(v)]))
type PIconName = IconName | keyof typeof rawExtra

const props = withDefaults(defineProps<{ name: PIconName; size?: number; stroke?: number; label?: string }>(), { size: 16, stroke: 1.75 })
const inner = computed(() => extra[props.name] ?? (shared as Record<string, string>)[props.name] ?? '')
</script>

<template>
  <svg class="icon" xmlns="http://www.w3.org/2000/svg" :width="size" :height="size" viewBox="0 0 24 24" fill="none" stroke="currentColor" :stroke-width="stroke"
    stroke-linecap="round" stroke-linejoin="round" :aria-hidden="label ? undefined : 'true'" :role="label ? 'img' : undefined" :aria-label="label" focusable="false" v-html="inner" />
</template>

<style scoped>
.icon { display: inline-block; vertical-align: middle; flex: none; }
</style>
