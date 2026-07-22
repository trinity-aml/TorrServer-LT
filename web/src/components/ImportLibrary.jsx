import { Button, Dialog, DialogActions, DialogContent, DialogTitle, TextField } from '@material-ui/core'
import ListItem from '@material-ui/core/ListItem'
import ListItemIcon from '@material-ui/core/ListItemIcon'
import ListItemText from '@material-ui/core/ListItemText'
import PublishIcon from '@material-ui/icons/Publish'
import { useState } from 'react'
import axios from 'axios'
import { useTranslation } from 'react-i18next'
import { torrentsHost } from 'utils/Hosts'

// Accepts any of the three export formats (magnets.txt / torrs.txt / library.json)
// and normalizes each entry to an importable { link, title, category, poster }.
const parseEntries = text => {
  const trimmed = (text || '').trim()
  if (!trimmed) return []

  if (trimmed.startsWith('[') || trimmed.startsWith('{')) {
    try {
      const parsed = JSON.parse(trimmed)
      const arr = Array.isArray(parsed) ? parsed : [parsed]
      return arr
        .filter(it => it && (it.hash || it.torrs_hash || it.link))
        .map(it => ({
          link:
            it.link ||
            (it.torrs_hash
              ? `torrs://${it.torrs_hash}`
              : `magnet:?xt=urn:btih:${it.hash}&dn=${encodeURIComponent(it.title || it.name || it.hash)}`),
          title: it.title || it.name || '',
          category: it.category || '',
          poster: it.poster || '',
        }))
    } catch {
      // not JSON — fall through to line parsing
    }
  }

  return trimmed
    .split(/\r?\n/)
    .map(l => l.trim())
    .filter(l => l.startsWith('magnet:') || l.startsWith('torrs://') || /^[0-9a-fA-F]{40}$/.test(l))
    .map(link => ({ link, title: '', category: '', poster: '' }))
}

export default function ImportLibrary({ isOffline, isLoading }) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState(null)

  const close = () => {
    setOpen(false)
    setText('')
    setResult(null)
  }

  const onFile = event => {
    const file = event.target.files && event.target.files[0]
    if (!file) return
    const reader = new FileReader()
    reader.onload = () => setText(String(reader.result || ''))
    reader.readAsText(file)
  }

  const runImport = async () => {
    const entries = parseEntries(text)
    if (!entries.length) {
      setResult({ ok: 0, fail: 0, total: 0 })
      return
    }
    setBusy(true)
    setResult(null)
    let ok = 0
    let fail = 0
    // Sequential add so a large library doesn't flood the server at once.
    for (const e of entries) {
      try {
        // eslint-disable-next-line no-await-in-loop
        await axios.post(torrentsHost(), {
          action: 'add',
          link: e.link,
          title: e.title,
          category: e.category,
          poster: e.poster,
          save_to_db: true,
        })
        ok += 1
      } catch {
        fail += 1
      }
    }
    setBusy(false)
    setResult({ ok, fail, total: entries.length })
  }

  return (
    <>
      <ListItem disabled={isOffline || isLoading} button key={t('ImportLibrary')} onClick={() => setOpen(true)}>
        <ListItemIcon>
          <PublishIcon />
        </ListItemIcon>
        <ListItemText primary={t('ImportLibrary')} />
      </ListItem>

      <Dialog open={open} onClose={close} maxWidth='sm' fullWidth>
        <DialogTitle>{t('ImportLibrary')}</DialogTitle>
        <DialogContent dividers>
          <div style={{ opacity: 0.7, marginBottom: 12 }}>{t('ImportLibraryHint')}</div>
          <input type='file' accept='.txt,.json,application/json,text/plain' onChange={onFile} />
          <TextField
            multiline
            minRows={4}
            maxRows={12}
            fullWidth
            variant='outlined'
            margin='normal'
            placeholder={t('ImportLibraryPlaceholder')}
            value={text}
            onChange={e => setText(e.target.value)}
          />
          {result && (
            <div style={{ marginTop: 8 }}>
              {t('ImportLibraryResult', {
                ok: result.ok,
                fail: result.fail,
                total: result.total,
              })}
            </div>
          )}
        </DialogContent>
        <DialogActions>
          <Button onClick={close} color='secondary' variant='outlined'>
            {t('Close')}
          </Button>
          <Button onClick={runImport} color='primary' variant='contained' disabled={busy || !text.trim()}>
            {busy ? t('ImportLibraryImporting') : t('Import')}
          </Button>
        </DialogActions>
      </Dialog>
    </>
  )
}
