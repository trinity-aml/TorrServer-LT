import { useState, useRef, useCallback } from 'react'
import Button from '@material-ui/core/Button'
import LinearProgress from '@material-ui/core/LinearProgress'
import { useTranslation } from 'react-i18next'
import { getTorrServerHost } from 'utils/Hosts'

import { SpeedSection, SpeedSizeRow, SpeedControlsRow, SpeedResult } from './style'

const SIZES = [
  { mb: 10, label: '10 MB' },
  { mb: 100, label: '100 MB' },
  { mb: 1024, label: '1 GB' },
]

const humanizeBytes = bytes => {
  if (bytes >= 1024 ** 3) return `${(bytes / 1024 ** 3).toFixed(2)} GB`
  return `${(bytes / 1024 ** 2).toFixed(1)} MB`
}

const clock = () => (typeof performance !== 'undefined' && performance.now ? performance.now() : Date.now())

// Client<->server throughput probe. Pulls the synthetic /download/<size> file
// (a zero-filled stream the backend generates on the fly) and times it. The
// body is streamed and its chunks discarded, so even a 1 GB run never buffers
// the whole file in memory; where the Streams API is missing we fall back to a
// single buffered read.
export default function SpeedTest() {
  const { t } = useTranslation()
  const [sizeMb, setSizeMb] = useState(10)
  const [running, setRunning] = useState(false)
  const [percent, setPercent] = useState(0)
  const [liveMbps, setLiveMbps] = useState(0)
  const [result, setResult] = useState(null)
  const [error, setError] = useState(false)
  const abortRef = useRef(null)

  const run = useCallback(async () => {
    setRunning(true)
    setResult(null)
    setError(false)
    setPercent(0)
    setLiveMbps(0)

    const controller = new AbortController()
    abortRef.current = controller
    const url = `${getTorrServerHost()}/download/${sizeMb}`
    const started = clock()
    const elapsed = () => Math.max(1, clock() - started)

    try {
      const resp = await fetch(url, { signal: controller.signal, cache: 'no-store' })
      if (!resp.ok) throw new Error(`status ${resp.status}`)

      const expected = Number(resp.headers.get('Content-Length')) || sizeMb * 1024 * 1024
      let received = 0

      if (resp.body && resp.body.getReader) {
        const reader = resp.body.getReader()
        for (;;) {
          // eslint-disable-next-line no-await-in-loop
          const { done, value } = await reader.read()
          if (done) break
          received += value.length
          setLiveMbps((received * 8) / (elapsed() / 1000) / 1e6)
          setPercent(Math.min(100, (received / expected) * 100))
        }
      } else {
        const buf = await resp.arrayBuffer()
        received = buf.byteLength
      }

      const elapsedMs = elapsed()
      setResult({ mbps: (received * 8) / (elapsedMs / 1000) / 1e6, elapsedMs, bytes: received })
      setPercent(100)
    } catch (e) {
      if (!(e && e.name === 'AbortError')) setError(true)
    } finally {
      setRunning(false)
      abortRef.current = null
    }
  }, [sizeMb])

  const cancel = useCallback(() => {
    if (abortRef.current) abortRef.current.abort()
  }, [])

  return (
    <SpeedSection>
      <span>{t('SpeedTest')}</span>

      <SpeedSizeRow>
        {SIZES.map(({ mb, label }) => (
          <Button
            key={mb}
            size='small'
            variant={sizeMb === mb ? 'contained' : 'outlined'}
            color='secondary'
            disabled={running}
            onClick={() => setSizeMb(mb)}
          >
            {label}
          </Button>
        ))}
      </SpeedSizeRow>

      <SpeedControlsRow>
        {running ? (
          <Button size='small' variant='contained' color='secondary' onClick={cancel}>
            {t('Cancel')}
          </Button>
        ) : (
          <Button size='small' variant='contained' color='secondary' onClick={run}>
            {t('SpeedTestRun')}
          </Button>
        )}

        <SpeedResult>
          {running && <span>{liveMbps.toFixed(1)} Mbps</span>}
          {!running && error && <span>{t('SpeedTestError')}</span>}
          {!running && !error && result && (
            <span>
              <b>{result.mbps.toFixed(1)} Mbps</b> · {(result.elapsedMs / 1000).toFixed(1)}s ·{' '}
              {humanizeBytes(result.bytes)}
            </span>
          )}
        </SpeedResult>
      </SpeedControlsRow>

      {running && <LinearProgress variant='determinate' value={percent} />}
    </SpeedSection>
  )
}
