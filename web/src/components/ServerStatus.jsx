import {
  Button,
  Chip,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  useMediaQuery,
} from '@material-ui/core'
import ListItem from '@material-ui/core/ListItem'
import ListItemIcon from '@material-ui/core/ListItemIcon'
import ListItemText from '@material-ui/core/ListItemText'
import AssessmentIcon from '@material-ui/icons/Assessment'
import { useState } from 'react'
import axios from 'axios'
import { useQuery } from 'react-query'
import { useTranslation } from 'react-i18next'
import { runtimeStatusHost } from 'utils/Hosts'
import { humanizeSize, humanizeSpeed } from 'utils/Utils'

const fetchRuntimeStatus = async () => {
  const { data } = await axios.get(runtimeStatusHost())
  return data
}

const Metric = ({ label, value }) => (
  <div style={{ minWidth: 120 }}>
    <div style={{ fontSize: 12, opacity: 0.6 }}>{label}</div>
    <div style={{ fontSize: 18, fontWeight: 600 }}>{value}</div>
  </div>
)

export default function ServerStatus({ isOffline, isLoading }) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const [showRaw, setShowRaw] = useState(false)
  const fullScreen = useMediaQuery('@media (max-width:930px)')

  const { data } = useQuery('runtimeStatus', fetchRuntimeStatus, {
    enabled: open,
    refetchInterval: open ? 2000 : false,
    retry: 1,
  })

  const bt = data?.bt || {}
  const torrents = bt.torrents || []

  const integration = (label, enabled) => (
    <Chip
      size='small'
      label={label}
      color={enabled ? 'primary' : 'default'}
      variant={enabled ? 'default' : 'outlined'}
      style={{ marginRight: 6, marginBottom: 6 }}
    />
  )

  return (
    <>
      <ListItem disabled={isOffline || isLoading} button key={t('ServerStatus')} onClick={() => setOpen(true)}>
        <ListItemIcon>
          <AssessmentIcon />
        </ListItemIcon>
        <ListItemText primary={t('ServerStatus')} />
      </ListItem>

      <Dialog open={open} onClose={() => setOpen(false)} fullScreen={fullScreen} maxWidth='lg' fullWidth>
        <DialogTitle>{t('ServerStatus')}</DialogTitle>
        <DialogContent dividers>
          {!data ? (
            <div style={{ opacity: 0.6 }}>…</div>
          ) : (
            <>
              <div style={{ marginBottom: 12 }}>
                {integration('DLNA', data.dlna_enabled)}
                {integration('Bonjour', data.bonjour_enabled)}
                {integration('WebDAV', data.webdav_enabled)}
                {integration('FUSE', data.fuse_enabled)}
                {data.friendly_name ? (
                  <Chip size='small' label={data.friendly_name} style={{ marginRight: 6, marginBottom: 6 }} />
                ) : null}
              </div>

              <div
                style={{
                  display: 'flex',
                  flexWrap: 'wrap',
                  gap: 16,
                  marginBottom: 16,
                }}
              >
                <Metric label={t('Torrents')} value={bt.torrent_count || 0} />
                <Metric label={t('ActiveStreams')} value={bt.active_streams || 0} />
                <Metric
                  label={t('Loaded')}
                  value={`${humanizeSize(bt.loaded_size) || 0} / ${humanizeSize(bt.total_size) || 0}`}
                />
                <Metric label={t('Peers')} value={`${bt.active_peers || 0} / ${bt.total_peers || 0}`} />
                <Metric label={t('Seeders')} value={bt.connected_seeders || 0} />
                <Metric label={`↓ ${t('Speed')}`} value={humanizeSpeed(bt.download_speed) || '0'} />
                <Metric label={`↑ ${t('Speed')}`} value={humanizeSpeed(bt.upload_speed) || '0'} />
                <Metric label={t('ListenPort')} value={bt.listen_port || t('Auto')} />
              </div>

              {torrents.length > 0 && (
                <Table size='small'>
                  <TableHead>
                    <TableRow>
                      <TableCell>{t('Title')}</TableCell>
                      <TableCell>{t('Status')}</TableCell>
                      <TableCell align='right'>{t('Loaded')}</TableCell>
                      <TableCell align='right'>{t('Peers')}</TableCell>
                      <TableCell align='right'>↓ / ↑</TableCell>
                    </TableRow>
                  </TableHead>
                  <TableBody>
                    {torrents.map(tr => (
                      <TableRow key={tr.hash}>
                        <TableCell>{tr.title || tr.name || tr.hash}</TableCell>
                        <TableCell>{tr.stat_string}</TableCell>
                        <TableCell align='right'>
                          {humanizeSize(tr.loaded_size) || 0} / {humanizeSize(tr.torrent_size) || 0}
                        </TableCell>
                        <TableCell align='right'>
                          {tr.active_peers || 0} / {tr.total_peers || 0} · {tr.connected_seeders || 0}
                        </TableCell>
                        <TableCell align='right'>
                          {humanizeSpeed(tr.download_speed) || '0'} / {humanizeSpeed(tr.upload_speed) || '0'}
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              )}

              <div style={{ marginTop: 16 }}>
                <Button size='small' onClick={() => setShowRaw(v => !v)}>
                  {showRaw ? t('HideRawStat') : t('ShowRawStat')}
                </Button>
                {showRaw && (
                  <pre
                    style={{
                      maxHeight: 320,
                      overflow: 'auto',
                      fontSize: 12,
                      background: 'rgba(127,127,127,0.1)',
                      padding: 12,
                      borderRadius: 8,
                      whiteSpace: 'pre-wrap',
                      wordBreak: 'break-word',
                    }}
                  >
                    {bt.raw_stat}
                  </pre>
                )}
              </div>
            </>
          )}
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
