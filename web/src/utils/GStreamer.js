import { useQuery } from 'react-query'

import { getTorrServerHost, gstSettingsHost } from './Hosts'

export const GST_RUNTIME_QUERY_KEY = 'gstreamer-runtime-settings'

const unavailableRuntime = { built_in: false }

const loadGStreamerRuntime = async () => {
  const response = await fetch(gstSettingsHost())
  if (!response.ok) return unavailableRuntime
  return response.json()
}

export const useGStreamerRuntime = () => {
  const { data } = useQuery(GST_RUNTIME_QUERY_KEY, loadGStreamerRuntime, {
    staleTime: 60 * 1000,
    cacheTime: 5 * 60 * 1000,
    retry: 1,
    refetchOnWindowFocus: false,
  })

  return data || unavailableRuntime
}

const fileExtension = path => {
  const fileName = path.split('?')[0]
  const dot = fileName.lastIndexOf('.')
  return dot === -1 ? '' : fileName.slice(dot + 1).toLowerCase()
}

export const shouldUseGStreamerPlayer = (path, runtime) => {
  if (!runtime?.built_in) return false

  switch (fileExtension(path)) {
    case 'mkv':
    case 'mk3d':
    case 'webm':
      return true
    case 'avi':
      return Boolean(runtime.config?.TranscodeAVI)
    default:
      return false
  }
}

// A gst task is one pipeline with one playback position, so two players on the
// same torrent would drag it back and forth and time each other's segments out.
// Each browser tab therefore asks for its own session; the server gives it a
// pipeline of its own and builds the playlist URLs around that id.
const SESSION_STORAGE_KEY = 'gstSessionId'

export const gstreamerSessionId = () => {
  try {
    let id = sessionStorage.getItem(SESSION_STORAGE_KEY)
    if (!id) {
      id = Math.random().toString(36).slice(2, 12)
      sessionStorage.setItem(SESSION_STORAGE_KEY, id)
    }
    return id
  } catch {
    // private mode or blocked storage: one shared pipeline, as before
    return ''
  }
}

export const gstreamerMasterUrl = (hash, fileID, audio = 0) => {
  const sid = gstreamerSessionId()
  return `${getTorrServerHost()}/gst/${encodeURIComponent(hash)}/master.m3u8?index=${encodeURIComponent(
    fileID,
  )}&audio=${encodeURIComponent(audio)}${sid ? `&sid=${encodeURIComponent(sid)}` : ''}`
}

export const gstreamerProbeUrl = (hash, fileID) =>
  `${getTorrServerHost()}/gst/${encodeURIComponent(hash)}/probe?index=${encodeURIComponent(fileID)}`

export const gstreamerHeartbeatUrl = hash => `${getTorrServerHost()}/gst/${encodeURIComponent(hash)}/heartbeat`
