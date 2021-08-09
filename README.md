# shunt

telegram-shunt

## Workflow

- src - исходник
- buffer - сюда копия (чтобы видеть обновления src) - без sourceLink; бот маппит альбомы
- shunt - сюда копия (чтобы видеть обновления src) - есть sourceLink; бот добавляет сообщения с кнопками
- copy - cюда forward руками через callback из buffer по sourceLink (из shunt) и по маппингу альбомов
- dst - сюда копия из buffer (чтобы видеть обновления src)
