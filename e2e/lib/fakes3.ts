import crypto from 'node:crypto';
import http from 'node:http';

// A FAKE S3 for one spec's upload store: path-style requests to one bucket,
// signed with one access key, holding objects in memory — what muster's
// upload store does to it for a few small files (PUT, a chunked PUT, HEAD,
// GET, DELETE, the bucket's HEAD and location; no lifecycle). It is
// ask-media.spec.ts's (and uploads.spec.ts's) fake, cut to that.
//
//   const s3 = await startFakeS3(port, ACCESS, 'muster-e2e');
//   … MUSTER_S3_ENDPOINT: `http://127.0.0.1:${port}`, MUSTER_S3_BUCKET: 'muster-e2e' …
//   await s3.close();

export interface FakeS3 {
  objects: Map<string, Buffer>;
  close(): Promise<void>;
}

const xmlErr = (res: http.ServerResponse, status: number, code: string) =>
  res.writeHead(status, { 'Content-Type': 'application/xml' }).end(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>${code}</Code><Message>${code}</Message></Error>`);

function unchunk(b: Buffer): Buffer {
  const out: Buffer[] = [];
  let at = 0;
  for (;;) {
    const eol = b.indexOf('\r\n', at);
    if (eol < 0) break;
    const size = parseInt(b.subarray(at, eol).toString('latin1').split(';')[0], 16);
    if (!size) break;
    out.push(b.subarray(eol + 2, eol + 2 + size));
    at = eol + 2 + size + 2;
  }
  return Buffer.concat(out);
}

const objHeaders = (b: Buffer) => ({
  'Content-Length': String(b.length),
  ETag: '"' + crypto.createHash('md5').update(b).digest('hex') + '"',
  'Last-Modified': new Date().toUTCString(),
  'Content-Type': 'application/octet-stream',
});

export async function startFakeS3(port: number, access: string, bucket: string): Promise<FakeS3> {
  const objects = new Map<string, Buffer>();
  const srv = http.createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => {
      let body = Buffer.concat(chunks);
      const [rawPath, query = ''] = (req.url ?? '').split('?');
      const p = decodeURIComponent(rawPath);
      if (!(req.headers.authorization ?? '').startsWith(`AWS4-HMAC-SHA256 Credential=${access}/`)) return xmlErr(res, 403, 'AccessDenied');
      if (!p.startsWith('/' + bucket)) return xmlErr(res, 404, 'NoSuchBucket');
      const key = p.slice(bucket.length + 1).replace(/^\//, '');
      if (key === '') {
        if (req.method === 'HEAD') return void res.writeHead(200).end();
        if (query.startsWith('lifecycle')) return xmlErr(res, 404, 'NoSuchLifecycleConfiguration');
        if (query.startsWith('location')) return void res.writeHead(200, { 'Content-Type': 'application/xml' }).end('<LocationConstraint>us-east-1</LocationConstraint>');
        return xmlErr(res, 400, 'NotImplemented');
      }
      if (req.method === 'PUT') {
        if (String(req.headers['x-amz-content-sha256'] ?? '').startsWith('STREAMING')) body = unchunk(body);
        objects.set(key, body);
        return void res.writeHead(200, { ETag: objHeaders(body).ETag }).end();
      }
      const b = objects.get(key);
      if (req.method === 'HEAD') return b ? void res.writeHead(200, objHeaders(b)).end() : void res.writeHead(404).end();
      if (req.method === 'GET') return b ? void res.writeHead(200, objHeaders(b)).end(b) : xmlErr(res, 404, 'NoSuchKey');
      if (req.method === 'DELETE') {
        objects.delete(key);
        return void res.writeHead(204).end();
      }
      res.writeHead(405).end();
    });
  });
  await new Promise<void>((r) => srv.listen(port, '127.0.0.1', () => r()));
  return { objects, close: () => new Promise((r) => srv.close(() => r())) };
}
