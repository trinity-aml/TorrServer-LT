import { GitHub as GitHubIcon, Telegram as TelegramIcon } from '@material-ui/icons'

import { LinkWrapper, LinkIcon } from './style'

export default function LinkComponent({ name, link }) {
  const isTelegram = !!link && /t\.me|telegram/i.test(link)
  return (
    <LinkWrapper isLink={!!link} href={link} target='_blank' rel='noreferrer'>
      {link && (
        <LinkIcon>
          {isTelegram ? <TelegramIcon /> : <GitHubIcon />}
        </LinkIcon>
      )}

      <div>{name}</div>
    </LinkWrapper>
  )
}
