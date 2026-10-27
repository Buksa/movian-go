/**
 * Cookie delete demo — exercises native/popup.deletecookies against
 * both stores: the webview (CEF) jar and the shared HTTP jar.
 *
 * Point TESTURL at anything that answers Set-Cookie. A minimal lab:
 *
 *   python3 -c '
 *   from http.server import BaseHTTPRequestHandler, HTTPServer
 *   class H(BaseHTTPRequestHandler):
 *       def do_GET(self):
 *           self.send_response(200)
 *           self.send_header("Content-Type", "text/html")
 *           self.send_header("X-Seen-Cookie", self.headers.get("Cookie", "-"))
 *           if self.path == "/set":
 *               self.send_header("Set-Cookie", "sess=abc; Path=/")
 *               self.send_header("Set-Cookie", "pref=dark; Path=/")
 *           self.end_headers()
 *           self.wfile.write(b"<html><body>cookie lab</body></html>")
 *   HTTPServer(("127.0.0.1", 18999), H).serve_forever()'
 *
 * then open cookiedelete:demo and read the CK lines on the console.
 */

var page  = require('movian/page');
var http  = require('movian/http');
var popup = require('native/popup');

var TESTURL = 'http://127.0.0.1:18999/set';   // answers Set-Cookie
var ECHOURL = 'http://127.0.0.1:18999/echo';  // echoes Cookie header

new page.Route('cookiedelete:demo', function(page) {
  page.metadata.title = 'Cookie delete demo';
  page.type = 'directory';
  page.loading = false;

  // --- webview (CEF) store ------------------------------------------
  popup.webbrowser(TESTURL, 'cookie demo');
  Core.sleep(4);                       // let the page load + store
  print('CK webview before: ' + JSON.stringify(popup.webcookies(TESTURL)));
  var n = popup.deletecookies(TESTURL);
  print('CK webview deleted: ' + n);
  print('CK webview after: ' + JSON.stringify(popup.webcookies(TESTURL)));

  // --- shared HTTP jar -----------------------------------------------
  popup.injectcookies(ECHOURL, {ckA: '1', ckB: '2'});
  var r1 = http.request(ECHOURL, {method: 'GET'});
  print('CK jar sent before: ' + r1.headers['X-Seen-Cookie']);
  var m = popup.deletecookies(ECHOURL, ['ckA']);        // names array
  print('CK jar named delete: ' + m);
  var r2 = http.request(ECHOURL, {method: 'GET'});
  print('CK jar sent after: ' + r2.headers['X-Seen-Cookie']);
  var m2 = popup.deletecookies(ECHOURL, {ckB: true});   // names object
  print('CK jar obj delete: ' + m2);
  var r3 = http.request(ECHOURL, {method: 'GET'});
  print('CK jar sent final: ' + r3.headers['X-Seen-Cookie']);
});
