# shunt

telegram-shunt

## Workflow

- Src - исходник
- From - копия (чтобы видеть обновления Src) - без sourceLink; бот маппит альбомы
- Main - копия (чтобы видеть обновления Src) - есть sourceLink; бот добавляет сообщения с кнопками
- CopyTo & Cancel - forward через callback из From по sourceLink (из Main) и по маппингу альбомов
- Dst - копия из From (чтобы видеть обновления Src) - без sourceLink
- Trash - копия из From (чтобы видеть обновления Src) - есть sourceLink

## Прочие соглашения

- Main & Cancel & Trash - по одному на всех
- копирование выполняет внешний копировальщик, бот выполняет только forward через callback
- sourceLink - последняя ссылка в сообщении, которую добавляет копировальщик при копировании
