/**
 * Property handling
 *
 * This module mirrors src/ecmascript/es_prop.c; the Go port's native bridge
 * is implemented in internal/ecmascript/es_prop.go.
 *
 * The general idea is that the native object (those accessed by
 * np.*) are not to be exposed to Javascript code directly.
 * But may only be passed as a proxied object
 *
 */

var np = require('native/prop');


/**
 * Proxyhandler for Prop
 */
var propHandler = {

  get: function(obj, name) {

    if(name === '__rawprop__') return obj;

    if(name == 'toString') return function() {
      return String(np.getValue(obj));
    }

    if(name == 'valueOf') return function() {
      return np.getValue(obj);
    }

    // Go: goja Proxy traps receive keys as strings. Convert only canonical array
    // indices for native/prop's indexed lookup; keep named children unchanged.
    if(typeof name === 'string') {
      var index = name >>> 0;
      if(String(index) === name && index !== 0xffffffff)
        name = index;
    }

    return makeProp(np.getChild(obj, name));
  },

  set: function(obj, name, value) {

    if(typeof value == 'object' && value !== null) {

      if('toRichString' in value) {
        np.setRichStr(obj, name, value.toRichString());
        return true;
      }

      if(np.isValue(value)) {
        np.set(obj, name, np.getValue(value));
        return true;
      }

      var x = np.getChild(obj, name);
      if(typeof x !== 'object')
        throw "Assignment to non directory prop";

      x = makeProp(x);
      for(var i in value)
        x[i] = value[i];

    } else {
      np.set(obj, name, value);
    }
    return true;
  },

  enumerate: np.enumerate,
  has: np.has,
  deleteProperty: np.deleteChild
}


/**
 * Helper to create a proxied version of raw showtime property object
 */
function makeProp(prop) {
  return new Proxy(prop, propHandler);
}


/**
 *
 */
function makeValue(type, v1, v2) {
  switch(type) {
  case 'set':
    return [v1];
  case 'uri':
    return [v1, v2];
  default:
    return [null];
  }
}

exports.__proto__ = np;

/**
 * Exported members
 */
exports.global = makeProp(np.global());
exports.makeProp = makeProp;

exports.createRoot = function(name) {
  return makeProp(np.create(name));
}

exports.subscribeValue = function(prop, callback, ctrl) {
  return np.subscribe(prop, function(type, v1, v2) {

    if(type == 'destroyed')
      return;

    callback.apply(null, makeValue(type, v1, v2));
  }, ctrl);
}
