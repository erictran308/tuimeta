//! Ogg Opus files (RFC 7845), for the user's own sounds for new messages
//! (`chime.rs`), decoded here in Rust as tuigram decodes voice messages.
//! The reader keeps only what one stream needs, so a file can't make it
//! hold much, and the decoder runs where its panics are caught
//! (`chime::ogg`).

use std::io::Read;

use anyhow::{Context, Result, bail};

use crate::opus::OpusDecoder;
use crate::sound::RATE;

/// Bytes a packet may take. Opus's are a few hundred, a lot of padding
/// included; the comment header, which can be bigger, is skipped unread.
const MAX_PACKET: usize = 64 << 10;

/// A packet of an Ogg stream.
struct Packet {
    data: Vec<u8>,
    /// The sound ends with it: the last page's last packet. Its granule
    /// position says where, maybe partway through it.
    end: Option<u64>,
}

/// The packets of an Ogg file's first stream (RFC 3533), read page by page.
/// Pages of other streams are passed over, and a packet is never more than
/// [`MAX_PACKET`], so what a file holds can't make this hold much. Page
/// checksums aren't checked: someone making a file to break tuimeta would
/// write right ones.
struct OggPackets<R: Read> {
    reader: R,
    /// The stream read, the first page's.
    serial: Option<u32>,
    /// The page read: its segments' lengths, how many were taken, its
    /// data and where the next segment starts in it.
    lacing: Vec<u8>,
    segment: usize,
    page: Vec<u8>,
    at: usize,
    /// The page's granule position, and whether it ends the stream.
    granule: u64,
    last_page: bool,
    /// The packet being put together, which may go on over pages.
    packet: Vec<u8>,
}

impl<R: Read> OggPackets<R> {
    fn new(reader: R) -> Self {
        Self {
            reader,
            serial: None,
            lacing: Vec::new(),
            segment: 0,
            page: Vec::new(),
            at: 0,
            granule: 0,
            last_page: false,
            packet: Vec::new(),
        }
    }

    /// The next packet, its data dropped unless `keep`; `None` at the end.
    fn next(&mut self, keep: bool) -> Result<Option<Packet>> {
        self.packet.clear();
        loop {
            if self.segment == self.lacing.len() {
                if !self.read_page()? {
                    return Ok(None);
                }
                continue;
            }
            let len = usize::from(self.lacing[self.segment]);
            self.segment += 1;
            let data = &self.page[self.at..self.at + len];
            self.at += len;
            if keep {
                if self.packet.len() + len > MAX_PACKET {
                    bail!("part of it is too big");
                }
                self.packet.extend_from_slice(data);
            }
            // A segment shorter than 255 bytes ends its packet.
            if len < 255 {
                let rest = &self.lacing[self.segment..];
                let last_on_page = rest.iter().all(|&l| l == 255);
                return Ok(Some(Packet {
                    data: std::mem::take(&mut self.packet),
                    end: (last_on_page && self.last_page).then_some(self.granule),
                }));
            }
        }
    }

    /// Reads the next page of the stream; false at the end of the file.
    fn read_page(&mut self) -> Result<bool> {
        loop {
            let mut header = [0; 27];
            let got = read_up_to(&mut self.reader, &mut header).context("it can't be read")?;
            if got == 0 {
                return Ok(false);
            }
            if &header[..4] != b"OggS" || header[4] != 0 {
                bail!("it isn't an Ogg file");
            }
            if got < header.len() {
                bail!("it's cut short");
            }
            let mut lacing = vec![0; usize::from(header[26])];
            self.reader
                .read_exact(&mut lacing)
                .context("it's cut short")?;
            let len = lacing.iter().map(|&l| usize::from(l)).sum();
            self.page.resize(len, 0);
            self.reader
                .read_exact(&mut self.page)
                .context("it's cut short")?;
            let serial = u32::from_le_bytes([header[14], header[15], header[16], header[17]]);
            if *self.serial.get_or_insert(serial) != serial {
                continue;
            }
            // A page that doesn't go on with the packet before means that
            // one was cut short; it's dropped.
            if header[5] & 1 == 0 {
                self.packet.clear();
            }
            self.granule = u64::from_le_bytes(header[6..14].try_into().expect("8 bytes"));
            self.last_page = header[5] & 4 != 0;
            (self.lacing, self.segment, self.at) = (lacing, 0, 0);
            return Ok(true);
        }
    }
}

/// Reads into `buf` until it's full or the file ends, returning how much
/// was read.
fn read_up_to(reader: &mut impl Read, buf: &mut [u8]) -> std::io::Result<usize> {
    let mut got = 0;
    while got < buf.len() {
        match reader.read(&mut buf[got..]) {
            Ok(0) => break,
            Ok(n) => got += n,
            Err(e) if e.kind() == std::io::ErrorKind::Interrupted => {}
            Err(e) => return Err(e),
        }
    }
    Ok(got)
}

/// The sound of an Ogg Opus file (RFC 7845), packet by packet, mixed down
/// to one channel.
pub struct OggOpus<R: Read> {
    packets: OggPackets<R>,
    decoder: OpusDecoder,
    /// Samples at the start that are the encoder's warm-up, not sound.
    skip: usize,
    /// The volume change the file asks for.
    gain: f32,
    /// Samples decoded so far, the warm-up included.
    decoded: u64,
    buffer: Vec<i16>,
}

impl<R: Read> OggOpus<R> {
    pub fn new(reader: R) -> Result<Self> {
        let mut packets = OggPackets::new(reader);
        let head = packets.next(true)?.context("it's empty")?;
        let h = &head.data;
        if h.len() < 19 || !h.starts_with(b"OpusHead") {
            bail!("it isn't Opus");
        }
        // Versions 0.x; a new major version may be laid out otherwise.
        if h[8] >> 4 != 0 {
            bail!("it's a newer kind of Opus file");
        }
        // Family 0 is mono or stereo; others have more channels.
        if h[18] != 0 || !(1..=2).contains(&h[9]) {
            bail!("it has more than two channels");
        }
        let gain = f32::from(i16::from_le_bytes([h[16], h[17]])) / 256.0;
        let skip = usize::from(u16::from_le_bytes([h[10], h[11]]));
        // The comment header: the encoder's name, maybe pictures.
        packets.next(false)?;
        Ok(Self {
            packets,
            decoder: OpusDecoder::new(RATE, 1)?,
            skip,
            gain: 10f32.powf(gain / 20.0),
            decoded: 0,
            buffer: vec![0; OpusDecoder::MAX_FRAME_SIZE_48K],
        })
    }

    /// The next stretch of sound, into `out` (maybe none); false once
    /// there's no more.
    pub fn next(&mut self, out: &mut Vec<i16>) -> Result<bool> {
        out.clear();
        let Some(packet) = self.packets.next(true)? else {
            return Ok(false);
        };
        let n = self
            .decoder
            .decode(&packet.data, &mut self.buffer)
            .context("part of it is broken")?;
        let n = n.min(self.buffer.len());
        let end = match packet.end {
            Some(granule) => granule.saturating_sub(self.decoded).min(n as u64) as usize,
            None => n,
        };
        self.decoded += n as u64;
        let start = self.skip.min(end);
        self.skip -= self.skip.min(n);
        let samples = &self.buffer[start..end];
        if self.gain == 1.0 {
            out.extend_from_slice(samples);
        } else {
            let scale = |s: i16| (f32::from(s) * self.gain).clamp(-32768.0, 32767.0) as i16;
            out.extend(samples.iter().map(|&s| scale(s)));
        }
        Ok(true)
    }
}

#[cfg(test)]
mod tests {
    use std::io::Cursor;

    use super::*;

    /// A second of a 440 Hz tone, as ffmpeg's libopus writes it.
    const TONE: &[u8] = include_bytes!("../testdata/voice.ogg");

    fn decode_all(bytes: &[u8]) -> Result<Vec<i16>> {
        decode(OggOpus::new(Cursor::new(bytes))?)
    }

    fn decode<R: Read>(mut sound: OggOpus<R>) -> Result<Vec<i16>> {
        let (mut all, mut some) = (Vec::new(), Vec::new());
        while sound.next(&mut some)? {
            all.extend_from_slice(&some);
        }
        Ok(all)
    }

    #[test]
    fn a_file_decodes_to_the_length_it_was_recorded() {
        let samples = decode_all(TONE).unwrap();
        assert_eq!(
            samples.len(),
            RATE as usize,
            "the warm-up and padding are cut"
        );
        let loudest = samples.iter().map(|s| s.unsigned_abs()).max().unwrap();
        assert!(loudest > 3000, "the tone is there: {loudest}");
    }

    /// An Ogg page of `serial`'s stream, with these segments and data.
    fn page(serial: u32, flags: u8, granule: u64, lacing: &[u8], data: &[u8]) -> Vec<u8> {
        let mut page = b"OggS\0".to_vec();
        page.push(flags);
        page.extend(granule.to_le_bytes());
        page.extend(serial.to_le_bytes());
        // The page's number and checksum, which aren't read.
        page.extend([0; 8]);
        page.push(lacing.len() as u8);
        page.extend(lacing);
        page.extend(data);
        page
    }

    /// Pages holding one packet of `len` bytes.
    fn packet_pages(serial: u32, len: usize) -> Vec<u8> {
        let (mut pages, mut left, mut flags) = (Vec::new(), len, 0);
        loop {
            let mut lacing = Vec::new();
            while lacing.len() < 255 && left >= 255 {
                lacing.push(255);
                left -= 255;
            }
            let done = lacing.len() < 255;
            if done {
                lacing.push(left as u8);
            }
            let size: usize = lacing.iter().map(|&l| usize::from(l)).sum();
            pages.extend(page(serial, flags, u64::MAX, &lacing, &vec![0; size]));
            flags = 1;
            if done {
                return pages;
            }
        }
    }

    /// The tone's pages: the identification header, the comment header,
    /// then the sound.
    fn tone_pages() -> (u32, Vec<&'static [u8]>) {
        let starts: Vec<usize> = (0..TONE.len())
            .filter(|&i| TONE[i..].starts_with(b"OggS"))
            .chain([TONE.len()])
            .collect();
        let pages = starts.windows(2).map(|w| &TONE[w[0]..w[1]]).collect();
        (u32::from_le_bytes(TONE[14..18].try_into().unwrap()), pages)
    }

    #[test]
    fn other_streams_in_the_file_are_passed_over_and_not_kept() {
        let (serial, pages) = tone_pages();
        let mut file = pages[0].to_vec();
        // Each a stream of its own, which a general reader keeps track of.
        for other in 1..20_000u32 {
            file.extend(page(serial ^ other, 2, 0, &[3], b"abc"));
        }
        pages[1..].iter().for_each(|p| file.extend(*p));
        assert_eq!(decode_all(&file).unwrap().len(), RATE as usize);
    }

    #[test]
    fn a_huge_comment_header_is_skipped_but_a_huge_packet_of_sound_refused() {
        let (serial, pages) = tone_pages();
        let mut file = pages[0].to_vec();
        file.extend(packet_pages(serial, 4 << 20));
        pages[2..].iter().for_each(|p| file.extend(*p));
        assert_eq!(decode_all(&file).unwrap().len(), RATE as usize);

        let mut file = [pages[0], pages[1]].concat();
        file.extend(packet_pages(serial, MAX_PACKET + 1));
        let refused = decode_all(&file).unwrap_err().to_string();
        assert_eq!(refused, "part of it is too big");
    }

    #[test]
    fn what_isnt_ogg_opus_is_refused_rather_than_played() {
        let refused = |bytes: &[u8]| decode_all(bytes).unwrap_err().to_string();
        assert_eq!(refused(b"ID3\x04 an mp3"), "it isn't an Ogg file");
        let mut vorbis = TONE.to_vec();
        let at = vorbis.windows(8).position(|w| w == b"OpusHead").unwrap();
        vorbis[at..at + 8].copy_from_slice(b"\x01vorbis\0");
        assert_eq!(refused(&vorbis), "it isn't Opus");
    }

    #[test]
    fn a_damaged_file_never_takes_tuimeta_down() {
        // Cut short anywhere: an error, or less sound.
        for cut in (0..TONE.len()).step_by(101) {
            let _ = decode_all(&TONE[..cut]);
        }
        // The decoder panics on some packets a file can hold; reading a
        // sound runs it where that's caught, which this does too.
        let mut packets = OggPackets::new(Cursor::new(TONE));
        packets.next(false).unwrap();
        packets.next(false).unwrap();
        let mut seed = 1u32;
        let mut buffer = vec![0; OpusDecoder::MAX_FRAME_SIZE_48K];
        for _ in 0..12 {
            let packet = packets.next(true).unwrap().unwrap().data;
            for at in 0..packet.len() {
                seed = seed.wrapping_mul(1_103_515_245).wrapping_add(12_345);
                let mut bad = packet.clone();
                bad[at] ^= (seed >> 16) as u8 | 1;
                let _ = crate::images::contained(|| {
                    let mut decoder = OpusDecoder::new(RATE, 1)?;
                    Ok(decoder.decode(&bad, &mut buffer)?)
                });
            }
        }
        assert!(!crate::images::panic_is_contained(), "only while decoding");
    }
}
