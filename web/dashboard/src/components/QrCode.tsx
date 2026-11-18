import { useMemo, useState } from 'react';
import { downloadBlob, qrFilename, qrPath, qrPngBlob, qrSvgMarkup, QR_DARK, QR_LIGHT } from '../lib/qr';
import { Icon } from './Icon';

/** A QR code made in the browser; the URL never leaves the page to draw it. */
export function QrCode({ text, label, large }: { text: string; label: string; large?: boolean }) {
  const { size, d } = useMemo(() => qrPath(text), [text]);
  return (
    <div className={`qr-wrap ${large ? 'large' : ''}`}>
      <svg className="qr" viewBox={`0 0 ${size} ${size}`} role="img" aria-label={label} shapeRendering="crispEdges" data-qr={text}>
        <title>{label}</title>
        <rect width={size} height={size} fill={QR_LIGHT} />
        <path d={d} fill={QR_DARK} />
      </svg>
    </div>
  );
}

export function QrDownloads({ text, name }: { text: string; name: string }) {
  const [failed, setFailed] = useState(false);
  const svg = () => downloadBlob(new Blob([qrSvgMarkup(text)], { type: 'image/svg+xml' }), qrFilename(name, 'svg'));
  const png = async () => {
    try {
      setFailed(false);
      downloadBlob(await qrPngBlob(text), qrFilename(name, 'png'));
    } catch {
      setFailed(true);
    }
  };
  return (
    <div className="stack-sm">
      <div className="actions">
        <button type="button" className="btn btn-small" onClick={svg}>
          <Icon name="download" />
          Download SVG
        </button>
        <button type="button" className="btn btn-small" onClick={png}>
          <Icon name="download" />
          Download PNG
        </button>
      </div>
      {failed ? <p className="error">This browser could not make the PNG. Download the SVG instead.</p> : null}
    </div>
  );
}
