# ASCII frame protocol

Termcall sends monochrome ASCII video over the unordered `ascii-video` WebRTC
data channel. Retransmissions are disabled: recent video is more useful than a
late frame. The sender retains at most one waiting frame and pauses sends while
the SCTP buffered amount is congested.

## Binary frame

All integers use network byte order.

| Field | Size | Value |
|---|---:|---|
| magic | 2 bytes | `TV` |
| version | 1 byte | `1` |
| frame type | 1 byte | `1` keyframe, `2` delta |
| sequence | 4 bytes | increasing per sender |
| timestamp | 8 bytes | Unix milliseconds |
| columns | 2 bytes | 1–200 |
| rows | 2 bytes | 1–80 |
| charset | 1 byte | `1`, built-in monochrome charset |
| color mode | 1 byte | `0`, monochrome |
| payload size | 4 bytes | payload bytes that follow |

The complete encoded frame is limited to 64 KiB. Cell bytes must be printable
ASCII, which also prevents frame data from introducing terminal controls.

A keyframe payload contains `columns * rows` cell bytes. A delta contains zero
or more runs:

| Run field | Size |
|---|---:|
| starting cell index | 4 bytes |
| run length | 2 bytes |
| replacement cells | run length bytes |

The receiver applies a delta only when its dimensions match and its sequence is
exactly one greater than the last complete frame. Otherwise it sends a reliable
`video.keyframe_request` control message. Senders also emit a keyframe at least
every two seconds.
