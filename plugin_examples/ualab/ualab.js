var page = require('movian/page');
var service = require('movian/service');
var http = require('movian/http');
var popup = require('native/popup');
var LAB = 'http://127.0.0.1:18999';
service.create('UA lab', 'ualab:run', 'other', true, '');
new page.Route('ualab:run', function (p) {
  p.type = 'directory';
  print('[ua] before popup: "' + popup.webuseragent() + '"');
  var r = popup.webpopup(LAB + '/start', 'ua lab', LAB + '/done');
  var sent = String(http.request(LAB + '/ua'));
  var reported = popup.webuseragent();
  print('[ua] webpopup: ' + r.result);
  print('[ua] CEF sent:       ' + sent);
  print('[ua] webuseragent(): ' + reported);
  print('[ua] equal: ' + (sent == reported));
});
