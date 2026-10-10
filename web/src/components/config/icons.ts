// Icons of the alerts/notifications/administration section that the shared set does not have (Lucide, ISC).
// Same look as components/icons/Icon.vue; kept separate so the shared icon map stays untouched.
import iMail from 'lucide-static/icons/mail.svg?raw'
import iWebhook from 'lucide-static/icons/webhook.svg?raw'
import iBellRing from 'lucide-static/icons/bell-ring.svg?raw'
import iMessageSquare from 'lucide-static/icons/message-square.svg?raw'
import iBellOff from 'lucide-static/icons/bell-off.svg?raw'
import iCheckCheck from 'lucide-static/icons/check-check.svg?raw'
import iUndo2 from 'lucide-static/icons/undo-2.svg?raw'
import iZap from 'lucide-static/icons/zap.svg?raw'
import iSiren from 'lucide-static/icons/siren.svg?raw'
import iUserPlus from 'lucide-static/icons/user-plus.svg?raw'
import iUserX from 'lucide-static/icons/user-x.svg?raw'
import iKey from 'lucide-static/icons/key.svg?raw'
import iShieldOff from 'lucide-static/icons/shield-off.svg?raw'
import iRotateCcw from 'lucide-static/icons/rotate-ccw.svg?raw'
import iListChecks from 'lucide-static/icons/list-checks.svg?raw'
import iGlobe from 'lucide-static/icons/globe.svg?raw'
import iTimer from 'lucide-static/icons/timer.svg?raw'
import iMoonStar from 'lucide-static/icons/moon-star.svg?raw'
import iPlug from 'lucide-static/icons/plug.svg?raw'
import iFlaskConical from 'lucide-static/icons/flask-conical.svg?raw'
import iRadio from 'lucide-static/icons/radio.svg?raw'
import iInbox from 'lucide-static/icons/inbox.svg?raw'
import iUserCog from 'lucide-static/icons/user-cog.svg?raw'
import iShield from 'lucide-static/icons/shield.svg?raw'
import iCalendar from 'lucide-static/icons/calendar.svg?raw'
import iFileKey from 'lucide-static/icons/file-key.svg?raw'
import iRepeat from 'lucide-static/icons/repeat.svg?raw'
import iCircleX from 'lucide-static/icons/circle-x.svg?raw'
import iHourglass from 'lucide-static/icons/hourglass.svg?raw'

const raw: Record<string, string> = {
  'mail': iMail,
  'webhook': iWebhook,
  'bell-ring': iBellRing,
  'message-square': iMessageSquare,
  'bell-off': iBellOff,
  'check-check': iCheckCheck,
  'undo-2': iUndo2,
  'zap': iZap,
  'siren': iSiren,
  'user-plus': iUserPlus,
  'user-x': iUserX,
  'key': iKey,
  'shield-off': iShieldOff,
  'rotate-ccw': iRotateCcw,
  'list-checks': iListChecks,
  'globe': iGlobe,
  'timer': iTimer,
  'moon-star': iMoonStar,
  'plug': iPlug,
  'flask-conical': iFlaskConical,
  'radio': iRadio,
  'inbox': iInbox,
  'user-cog': iUserCog,
  'shield': iShield,
  'calendar': iCalendar,
  'file-key': iFileKey,
  'repeat': iRepeat,
  'circle-x': iCircleX,
  'hourglass': iHourglass,
}
export type CfgIconName = 'mail' | 'webhook' | 'bell-ring' | 'message-square' | 'bell-off' | 'check-check' | 'undo-2' | 'zap' | 'siren' | 'user-plus' | 'user-x' | 'key' | 'shield-off' | 'rotate-ccw' | 'list-checks' | 'globe' | 'timer' | 'moon-star' | 'plug' | 'flask-conical' | 'radio' | 'inbox' | 'user-cog' | 'shield' | 'calendar' | 'file-key' | 'repeat' | 'circle-x' | 'hourglass'
/** Inner SVG markup (the <svg> wrapper stripped). */
export const cfgIcons: Record<string, string> = Object.fromEntries(Object.entries(raw).map(([k, v]) => [k, v.replace(/<!--[\s\S]*?-->/g, '').replace(/^[\s\S]*?<svg[^>]*>/, '').replace(/<\/svg>\s*$/, '')]))
