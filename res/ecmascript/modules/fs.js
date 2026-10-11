

exports.writeFileSync = function(filename, data, opts) {
  var fs = require('native/fs');
  var fd = fs.open(filename, 'w');
  try {
    fs.write(fd, data, 0, null, 0);
  }
  finally {
    Core.resourceDestroy(fd);
  }
}

// C SDK: res/ecmascript/modules/fs.js returns a Duktape.Buffer.
// Go: use Uint8Array for native reads and return its byte buffer. gaftape's
// ArrayBuffer.toString shim decodes bytes like Duktape.Buffer; returning the
// buffer preserves that SDK contract without decoding to a string here.
exports.readFileSync = function(filename, opts) {
  var fs = require('native/fs');
  var fd = fs.open(filename, 'r');
  try {
    var buf = new Uint8Array(fs.fsize(fd));
    fs.read(fd, buf, 0, buf.length, 0);
    return buf.buffer;
  } finally {
    Core.resourceDestroy(fd);
  }
}
