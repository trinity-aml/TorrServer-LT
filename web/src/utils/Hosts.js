const { protocol, hostname, port } = window.location

let torrserverHost = process.env.REACT_APP_SERVER_HOST || `${protocol}//${hostname}${port ? `:${port}` : ''}`

export const torrentsHost = () => `${torrserverHost}/torrents`
export const viewedHost = () => `${torrserverHost}/viewed`
export const cacheHost = () => `${torrserverHost}/cache`
export const torrentUploadHost = () => `${torrserverHost}/torrent/upload`
export const settingsHost = () => `${torrserverHost}/settings`
export const streamHost = () => `${torrserverHost}/stream`
export const shutdownHost = () => `${torrserverHost}/shutdown`
export const echoHost = () => `${torrserverHost}/echo`
export const playlistTorrHost = () => `${torrserverHost}/stream`
export const torznabSearchHost = () => `${torrserverHost}/torznab/search`
export const searchHost = () => `${torrserverHost}/search`
export const torznabTestHost = () => `${torrserverHost}/torznab/test`
export const tmdbSettingsHost = () => `${torrserverHost}/tmdb/settings`
export const gstSettingsHost = () => `${torrserverHost}/gst/settings`

export const getTorrServerHost = () => torrserverHost
export const setTorrServerHost = host => {
  torrserverHost = host
}

// Per-tab playback-session token, generated once per page load. The dozens of
// connections a player opens from a direct &play link all carry it, so the cache
// groups them into one sliding window; another device (its own page load / its
// own playlist fetch) gets a different token and thus an isolated window even
// behind the same IP. The m3u playlist gets its own token server-side. See
// torr.streamGroupKey.
const sessionTokenValue = (
  (window.crypto && window.crypto.randomUUID && window.crypto.randomUUID()) ||
  Math.random().toString(36).slice(2, 12) + Date.now().toString(36)
).replace(/-/g, '')
export const sessionToken = () => sessionTokenValue
