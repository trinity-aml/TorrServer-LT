import { Button, Dialog, DialogActions, DialogContent, DialogTitle } from '@material-ui/core'
import ListItem from '@material-ui/core/ListItem'
import ListItemIcon from '@material-ui/core/ListItemIcon'
import ListItemText from '@material-ui/core/ListItemText'
import SaveAltIcon from '@material-ui/icons/SaveAlt'
import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { getTorrents } from 'utils/Utils'

const downloadText = (filename, text, mime) => {
  const blob = new Blob([text], { type: mime })
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = filename
  anchor.rel = 'noopener'
  document.body.appendChild(anchor)
  anchor.click()
  document.body.removeChild(anchor)
  URL.revokeObjectURL(url)
}

// Prefer the server-packed torrs token (carries embedded metadata); fall back to
// a bare infohash link. Both are importable via the /torrents add action.
const torrsLink = tr => `torrs://${(tr.torrs_hash || tr.hash || '').trim()}`
const magnetLink = tr => `magnet:?xt=urn:btih:${tr.hash}&dn=${encodeURIComponent(tr.title || tr.name || tr.hash)}`

export default function ExportLibrary({ isOffline, isLoading }) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const [torrents, setTorrents] = useState([])

  const openDialog = async () => {
    setOpen(true)
    try {
      const list = await getTorrents()
      setTorrents(Array.isArray(list) ? list : [])
    } catch {
      setTorrents([])
    }
  }

  const magnets = useMemo(() => torrents.map(magnetLink).join('\n'), [torrents])
  const torrs = useMemo(
    () =>
      torrents
        .filter(tr => tr.hash)
        .map(torrsLink)
        .join('\n'),
    [torrents],
  )
  const json = useMemo(
    () =>
      JSON.stringify(
        torrents.map(tr => ({
          hash: tr.hash,
          title: tr.title,
          name: tr.name,
          category: tr.category,
          poster: tr.poster,
          torrs_hash: tr.torrs_hash,
          torrent_size: tr.torrent_size,
        })),
        null,
        2,
      ),
    [torrents],
  )

  const empty = torrents.length === 0

  return (
    <>
      <ListItem disabled={isOffline || isLoading} button key={t('ExportLibrary')} onClick={openDialog}>
        <ListItemIcon>
          <SaveAltIcon />
        </ListItemIcon>
        <ListItemText primary={t('ExportLibrary')} />
      </ListItem>

      <Dialog open={open} onClose={() => setOpen(false)} maxWidth='xs' fullWidth>
        <DialogTitle>{t('ExportLibrary')}</DialogTitle>
        <DialogContent dividers>
          <div style={{ opacity: 0.7, marginBottom: 12 }}>{t('ExportLibraryCount', { count: torrents.length })}</div>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
            <Button
              variant='outlined'
              disabled={empty}
              onClick={() => downloadText('torrserver-magnets.txt', magnets, 'text/plain;charset=utf-8')}
            >
              {t('ExportMagnets')}
            </Button>
            <Button
              variant='outlined'
              disabled={empty}
              onClick={() => downloadText('torrserver-torrs.txt', torrs, 'text/plain;charset=utf-8')}
            >
              {t('ExportTorrs')}
            </Button>
            <Button
              variant='outlined'
              disabled={empty}
              onClick={() => downloadText('torrserver-library.json', json, 'application/json;charset=utf-8')}
            >
              {t('ExportJson')}
            </Button>
          </div>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setOpen(false)} color='primary' variant='contained'>
            {t('Close')}
          </Button>
        </DialogActions>
      </Dialog>
    </>
  )
}
