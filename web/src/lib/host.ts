/** The address the console is served from (shown on the sign-in page so operators know which server). */
export const instanceHost = () => (typeof location !== 'undefined' ? location.host : '')
