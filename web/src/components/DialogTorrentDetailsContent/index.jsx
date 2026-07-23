import { NoImageIcon } from 'icons'
import { humanizeSize, removeRedundantCharacters } from 'utils/Utils'
import { useEffect, useState } from 'react'
import { Button, ButtonGroup } from '@material-ui/core'
import ptt from 'parse-torrent-title'
import axios from 'axios'
import { viewedHost, torrentsHost } from 'utils/Hosts'
import { GETTING_INFO } from 'torrentStates'
import CircularProgress from '@material-ui/core/CircularProgress'
import { useTranslation } from 'react-i18next'

import { useUpdateCache, useGetSettings } from './customHooks'
import DialogHeader from './DialogHeader'
import TorrentCache from './TorrentCache'
import Table from './Table'
import DetailedView from './DetailedView'
import {
  DialogContentGrid,
  MainSection,
  Poster,
  SectionTitle,
  SectionSubName,
  WidgetWrapper,
  LoadingProgress,
  SectionHeader,
  CacheSection,
  TorrentFilesSection,
  Divider,
} from './style'
import { DownlodSpeedWidget, UploadSpeedWidget, PeersWidget, SizeWidget, StatusWidget, CategoryWidget } from './widgets'
import TorrentFunctions from './TorrentFunctions'
import { isFilePlayable } from './helpers'

const Loader = () => (
  <div style={{ minHeight: '80vh', display: 'grid', placeItems: 'center' }}>
    <CircularProgress color='secondary' />
  </div>
)

export default function DialogTorrentDetailsContent({ closeDialog, torrent }) {
  const { t } = useTranslation()
  const [isLoading, setIsLoading] = useState(true)
  const [isDetailedCacheView, setIsDetailedCacheView] = useState(false)
  const [viewedFileList, setViewedFileList] = useState()
  const [playableFileList, setPlayableFileList] = useState()
  const [seasonAmount, setSeasonAmount] = useState(null)
  const [selectedSeason, setSelectedSeason] = useState()
  const [isSnakeDebugMode] = useState(JSON.parse(localStorage.getItem('isSnakeDebugMode')) || false)

  const {
    poster,
    hash,
    title,
    category,
    name,
    stat,
    download_speed: downloadSpeed,
    upload_speed: uploadSpeed,
    torrent_size: torrentSize,
    file_stats: torrentFileList,
  } = torrent

  const cache = useUpdateCache(hash)
  const settings = useGetSettings(cache)

  const { Capacity, PiecesCount, PiecesLength, Filled } = cache

  useEffect(() => {
    if (playableFileList && seasonAmount === null) {
      const seasons = []
      playableFileList.forEach(({ path }) => {
        const currentSeason = ptt.parse(path).season
        if (currentSeason) {
          !seasons.includes(currentSeason) && seasons.push(currentSeason)
        }
      })
      seasons.length && setSelectedSeason(seasons[0])
      setSeasonAmount(seasons.sort((a, b) => a - b))
    }
  }, [playableFileList, seasonAmount])

  useEffect(() => {
    setPlayableFileList(torrentFileList?.filter(({ path }) => isFilePlayable(path)))
  }, [torrentFileList])

  useEffect(() => {
    const cacheLoaded = !!Object.entries(cache).length
    // A dropped torrent is IN_DB (its info — title/files — still comes from the
    // list and the cache endpoint serves it from the DB). Render it instead of
    // spinning forever: opening the details no longer wakes the torrent into the
    // session (so its cache can drop after playback), so we must not wait for it
    // to leave IN_DB. Only GETTING_INFO (no metadata yet) still blocks.
    const torrentLoaded = stat !== GETTING_INFO

    if (!cacheLoaded && !isLoading) setIsLoading(true)
    if (cacheLoaded && isLoading && torrentLoaded) setIsLoading(false)
  }, [stat, cache, isLoading])

  useEffect(() => {
    // getting viewed file list
    axios.post(viewedHost(), { action: 'list', hash }).then(({ data }) => {
      if (data) {
        const lst = data.map(itm => itm.file_index).sort((a, b) => a - b)
        setViewedFileList(lst)
      } else setViewedFileList()
    })
  }, [hash])

  useEffect(() => {
    // A DB record without a file list (e.g. added by a client that didn't cache
    // TorrServer.Files in Data and stored no info-dict — Lampa, an import, an
    // older build) would leave this dialog on "no playable files" forever.
    // `torrents get` makes the server promote the record into the session and
    // fetch metadata from the swarm (torr.GetTorrent); the 1s list poll then
    // delivers file_stats. Poke every 5s while the list is still empty — a
    // single call isn't enough because a promoted torrent with no active reader
    // auto-drops (TorrentDisconnectTimeout, up to 60s), which would abort a slow
    // metadata fetch. Each poke also refreshes the torrent's expiry, keeping it
    // alive until the info-dict arrives — including while it sits in
    // GETTING_INFO (metadata in flight), where the dialog shows its spinner. The
    // ONLY stop condition is file_stats appearing: once they do, the prop
    // updates, this effect re-runs, the guard returns early and the cleanup
    // clears the interval.
    if (torrentFileList?.length) return undefined
    const poke = () => axios.post(torrentsHost(), { action: 'get', hash }).catch(() => {})
    poke()
    const timer = setInterval(poke, 5000)
    return () => clearInterval(timer)
  }, [hash, torrentFileList])

  // Cache section: show the configured cache size (from settings, falls back to the
  // live cache Capacity) as the total, and how much of it is currently filled.
  const cacheSize = settings?.CacheSize || Capacity || 0

  const getParsedTitle = () => {
    const newNameStringArr = []

    const torrentParsedName = name && ptt.parse(name)

    if (title !== name) {
      newNameStringArr.push(removeRedundantCharacters(title))
    } else if (torrentParsedName?.title) newNameStringArr.push(removeRedundantCharacters(torrentParsedName?.title))

    // These 2 checks are needed to get year and resolution from torrent name if title does not have this info
    if (torrentParsedName?.year && !newNameStringArr[0].includes(torrentParsedName?.year))
      newNameStringArr.push(torrentParsedName?.year)
    if (torrentParsedName?.resolution && !newNameStringArr[0].includes(torrentParsedName?.resolution))
      newNameStringArr.push(torrentParsedName?.resolution)

    const newNameString = newNameStringArr.join('. ')

    // removeRedundantCharacters is returning ".." if it was "..."
    const lastDotShouldBeAdded =
      newNameString[newNameString.length - 1] === '.' && newNameString[newNameString.length - 2] === '.'

    return lastDotShouldBeAdded ? `${newNameString}.` : newNameString
  }

  return (
    <>
      <DialogHeader
        onClose={closeDialog}
        title={isDetailedCacheView ? t('DetailedCacheView.header') : t('TorrentDetails')}
        {...(isDetailedCacheView && { onBack: () => setIsDetailedCacheView(false) })}
      />

      <div
        style={{
          minHeight: '80vh',
          overflow: 'auto',
          ...(isDetailedCacheView && { display: 'flex', flexDirection: 'column' }),
        }}
      >
        {isLoading ? (
          <Loader />
        ) : isDetailedCacheView ? (
          <DetailedView
            downloadSpeed={downloadSpeed}
            uploadSpeed={uploadSpeed}
            torrent={torrent}
            torrentSize={torrentSize}
            PiecesCount={PiecesCount}
            PiecesLength={PiecesLength}
            stat={stat}
            cache={cache}
          />
        ) : (
          <DialogContentGrid>
            <MainSection>
              <Poster poster={poster}>{poster ? <img alt='poster' src={poster} /> : <NoImageIcon />}</Poster>

              <div>
                {title && name && name !== title ? (
                  getParsedTitle().length > 90 ? (
                    <>
                      <SectionTitle>{ptt.parse(name).title}</SectionTitle>
                      <SectionSubName mb={20}>{getParsedTitle()}</SectionSubName>
                    </>
                  ) : (
                    <>
                      <SectionTitle>{getParsedTitle()}</SectionTitle>
                      <SectionSubName mb={20}>{ptt.parse(name || '')?.title}</SectionSubName>
                    </>
                  )
                ) : (
                  <SectionTitle mb={20}>{getParsedTitle()}</SectionTitle>
                )}

                <WidgetWrapper>
                  <DownlodSpeedWidget data={downloadSpeed} />
                  <UploadSpeedWidget data={uploadSpeed} />
                  <PeersWidget data={torrent} />
                  <SizeWidget data={torrentSize} />
                  <StatusWidget stat={stat} />
                  <CategoryWidget data={category} />
                </WidgetWrapper>

                <Divider />

                <TorrentFunctions
                  hash={hash}
                  viewedFileList={viewedFileList}
                  playableFileList={playableFileList}
                  name={name}
                  title={title}
                  setViewedFileList={setViewedFileList}
                />
              </div>
            </MainSection>

            <CacheSection>
              <SectionHeader>
                <SectionTitle mb={20}>{t('Buffer')}</SectionTitle>
                <LoadingProgress
                  value={Filled}
                  style={{ marginTop: '5px' }}
                  fullAmount={cacheSize}
                  label={`${humanizeSize(cacheSize)} / ${humanizeSize(Filled) || `0 ${t('B')}`}`}
                />
              </SectionHeader>

              <TorrentCache isMini cache={cache} isSnakeDebugMode={isSnakeDebugMode} />
              <Button
                style={{ marginTop: '30px' }}
                variant='contained'
                color='primary'
                size='large'
                onClick={() => setIsDetailedCacheView(true)}
              >
                {t('DetailedCacheView.button')}
              </Button>
            </CacheSection>

            <TorrentFilesSection>
              <SectionTitle mb={20}>{t('TorrentContent')}</SectionTitle>

              {seasonAmount?.length > 1 && (
                <>
                  <SectionSubName mb={7}>{t('SelectSeason')}</SectionSubName>
                  <ButtonGroup style={{ marginBottom: '30px' }} color='secondary'>
                    {seasonAmount.map(season => (
                      <Button
                        key={season}
                        variant={selectedSeason === season ? 'contained' : 'outlined'}
                        onClick={() => setSelectedSeason(season)}
                      >
                        {season}
                      </Button>
                    ))}
                  </ButtonGroup>

                  <SectionTitle mb={20}>
                    {t('Season')} {selectedSeason}
                  </SectionTitle>
                </>
              )}

              <Table
                hash={hash}
                playableFileList={playableFileList}
                viewedFileList={viewedFileList}
                selectedSeason={selectedSeason}
                seasonAmount={seasonAmount}
              />
            </TorrentFilesSection>
          </DialogContentGrid>
        )}
      </div>
    </>
  )
}
