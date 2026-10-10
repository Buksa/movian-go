package com.moviango.mediaplayer;

import android.content.Context;
import android.content.Intent;
import android.database.Cursor;
import android.media.tv.TvContract;
import android.media.tv.TvInputInfo;
import android.media.tv.TvInputManager;
import android.net.Uri;
import android.util.Log;

import java.io.File;
import java.io.FileInputStream;

import org.json.JSONArray;
import org.json.JSONObject;

// TIF bridge (android.media.tv): enumerates TvInputServices and their
// channels from the system TvProvider, and drives the TvView owned by
// GLWActivity. Works on every Android TV where an input is present —
// OEM tuner inputs, IPTV inputs, streaming inputs. New in Go.
public class Tv {

    private static Context ctx;
    private static GLWActivity act;

    public static void init(Context c) {
        ctx = c.getApplicationContext();
    }

    // attach/detach — GLWActivity owns the TvView; the static link lets
    // backend threads (via Tv.tune/untune) reach it without a provider
    // handle. Single-activity app, so one slot is enough.
    public static void attach(GLWActivity a) { act = a; }
    public static void detach(GLWActivity a) { if (act == a) act = null; }

    private static TvInputManager tvInputManager() {
        if (ctx == null)
            return null;
        return (TvInputManager) ctx.getSystemService(
            Context.TV_INPUT_SERVICE);
    }

    // inputCount — >0 when at least one TvInputService is registered
    // (any app can be an input). 0 on non-TV devices.
    public static int inputCount() {
        TvInputManager m = tvInputManager();
        if (m == null)
            return 0;
        try {
            return m.getTvInputList().size();
        } catch (Exception e) {
            return 0;
        }
    }

    // listInputs — JSON array of {id,label,passthrough,setup}.
    public static String listInputs() {
        JSONArray arr = new JSONArray();
        TvInputManager m = tvInputManager();
        if (m == null)
            return arr.toString();
        try {
            for (TvInputInfo i : m.getTvInputList()) {
                JSONObject o = new JSONObject();
                o.put("id", i.getId());
                CharSequence l = i.loadLabel(ctx);
                o.put("label", l != null ? l.toString() : i.getId());
                o.put("passthrough", i.isPassthroughInput());
                Intent s = i.createSetupIntent();
                o.put("setup", s != null);
                arr.put(o);
            }
        } catch (Exception e) {
            Log.e("Movian", "Tv.listInputs failed", e);
        }
        Log.d("Movian", "Tv.listInputs: " + arr.length() + " inputs");
        return arr.toString();
    }

    // listChannels — JSON array of {id,inputId,name,number} for every
    // browsable channel in the system TvProvider, whatever input owns
    // it (tuner DVB, IPTV, streaming).
    public static String listChannels() {
        JSONArray arr = new JSONArray();
        if (ctx == null)
            return arr.toString();
        String[] proj = {
            TvContract.Channels._ID,
            TvContract.Channels.COLUMN_INPUT_ID,
            TvContract.Channels.COLUMN_DISPLAY_NAME,
            TvContract.Channels.COLUMN_DISPLAY_NUMBER,
            TvContract.Channels.COLUMN_SERVICE_TYPE,
            TvContract.Channels.COLUMN_BROWSABLE,
        };
        Cursor c = null;
        try {
            // TvProvider rejects a caller-side selection unless the app
            // holds ACCESS_ALL_EPG_DATA (signature|privileged, not
            // grantable to us). Query unfiltered and filter client-side.
            c = ctx.getContentResolver().query(
                TvContract.Channels.CONTENT_URI, proj,
                null, null,
                TvContract.Channels.COLUMN_DISPLAY_NUMBER);
            while (c != null && c.moveToNext()) {
                if (!c.isNull(5) && c.getInt(5) == 0)
                    continue; // not browsable
                JSONObject o = new JSONObject();
                o.put("id", c.getLong(0));
                o.put("inputId", c.getString(1));
                String n = c.getString(2);
                o.put("name", n != null ? n : "");
                String num = c.getString(3);
                o.put("number", num != null ? num : "");
                arr.put(o);
            }
        } catch (Exception e) {
            Log.e("Movian", "Tv.listChannels failed", e);
        } finally {
            if (c != null)
                c.close();
        }
        if (arr.length() == 0)
            mergeDiscoveredChannels(arr);
        Log.d("Movian", "Tv.listChannels: " + arr.length() + " channels");
        return arr.toString();
    }

    // mergeDiscoveredChannels — on boxes where the vendor publishes
    // channels under package-locked input_ids we can't read (e.g. TIM:
    // ACCESS_ALL_EPG_DATA is privileged-only), a discovered channel map
    // can be dropped at <externalFilesDir>/tv_channels.json. Entries
    // carry the vendor feeder URI so tuning bypasses TvProvider rowids.
    private static void mergeDiscoveredChannels(JSONArray arr) {
        File dir = ctx.getExternalFilesDir(null);
        if (dir == null)
            return;
        File f = new File(dir, "tv_channels.json");
        if (!f.isFile())
            return;
        try {
            byte[] buf = new byte[(int) f.length()];
            FileInputStream in = new FileInputStream(f);
            int off = 0, n;
            while (off < buf.length &&
                   (n = in.read(buf, off, buf.length - off)) > 0)
                off += n;
            in.close();
            JSONArray chans = new JSONObject(new String(buf, "UTF-8"))
                .optJSONArray("channels");
            if (chans == null)
                return;
            for (int i = 0; i < chans.length(); i++)
                arr.put(chans.getJSONObject(i));
            Log.d("Movian", "Tv: merged " + chans.length() +
                  " discovered channels");
        } catch (Exception e) {
            Log.e("Movian", "Tv.mergeDiscoveredChannels failed", e);
        }
    }

    // programs — now/next EPG rows for a channel as JSON
    // [{id,title,desc,start,end}] (start/end = unix ms). Same
    // permission wall as listChannels: readable only when Movian can
    // see the channel (own input, or privileged build) — returns []
    // elsewhere, callers fall back to XMLTV.
    public static String programs(long channelId) {
        JSONArray arr = new JSONArray();
        if (ctx == null)
            return arr.toString();
        String[] proj = {
            TvContract.Programs._ID,
            TvContract.Programs.COLUMN_CHANNEL_ID,
            TvContract.Programs.COLUMN_TITLE,
            TvContract.Programs.COLUMN_SHORT_DESCRIPTION,
            TvContract.Programs.COLUMN_START_TIME_UTC_MILLIS,
            TvContract.Programs.COLUMN_END_TIME_UTC_MILLIS,
        };
        Cursor c = null;
        try {
            c = ctx.getContentResolver().query(
                TvContract.Programs.CONTENT_URI, proj,
                null, null,
                TvContract.Programs.COLUMN_START_TIME_UTC_MILLIS);
            long now = System.currentTimeMillis();
            while (c != null && c.moveToNext()) {
                if (c.getLong(1) != channelId)
                    continue;
                if (c.getLong(5) <= now)
                    continue; // fully past
                JSONObject o = new JSONObject();
                o.put("id", c.getLong(0));
                String t = c.getString(2);
                o.put("title", t != null ? t : "");
                String d = c.getString(3);
                o.put("desc", d != null ? d : "");
                o.put("start", c.getLong(4));
                o.put("stop", c.getLong(5));
                arr.put(o);
            }
        } catch (Exception e) {
            Log.e("Movian", "Tv.programs failed", e);
        } finally {
            if (c != null)
                c.close();
        }
        return arr.toString();
    }

    // epgUrl — optional XMLTV feed URL from the discovered-channels
    // sidecar (<externalFilesDir>/tv_channels.json, "epgUrl" key).
    public static String epgUrl() {
        if (ctx == null)
            return "";
        File dir = ctx.getExternalFilesDir(null);
        if (dir == null)
            return "";
        File f = new File(dir, "tv_channels.json");
        if (!f.isFile())
            return "";
        try {
            byte[] buf = new byte[(int) f.length()];
            FileInputStream in = new FileInputStream(f);
            int off = 0, n;
            while (off < buf.length &&
                   (n = in.read(buf, off, buf.length - off)) > 0)
                off += n;
            in.close();
            String u = new JSONObject(new String(buf, "UTF-8"))
                .optString("epgUrl");
            return u != null ? u : "";
        } catch (Exception e) {
            return "";
        }
    }

    // tune/untune — dispatch onto the UI thread; the TvView itself
    // lives in GLWActivity (view hierarchy owner).
    public static void tune(final String inputId, final long channelId) {
        GLWActivity a = act;
        if (a == null)
            return;
        a.runOnUiThread(new Runnable() {
            public void run() { a.tvTune(inputId, channelId); }
        });
    }

    // tuneUri — tune by raw URI string (vendor-specific schemes like
    // dvb://<triplet> that bypass TvProvider rowids).
    public static void tuneUri(final String inputId, final String uri) {
        GLWActivity a = act;
        if (a == null)
            return;
        a.runOnUiThread(new Runnable() {
            public void run() { a.tvTuneUri(inputId, Uri.parse(uri)); }
        });
    }

    public static void untune() {
        GLWActivity a = act;
        if (a == null)
            return;
        a.runOnUiThread(new Runnable() {
            public void run() { a.tvUntune(); }
        });
    }

    // setVideoRect — move the TvView surface (pixels). An empty or
    // negative rect means fullscreen; the rect persists across
    // untune/retune so the channel-list PiP survives re-tunes.
    public static void setVideoRect(final int l, final int t,
                                    final int r, final int b) {
        GLWActivity a = act;
        if (a == null)
            return;
        a.runOnUiThread(new Runnable() {
            public void run() { a.tvSetVideoRect(l, t, r, b); }
        });
    }

    public static int isTuned() {
        String s = uiCall(new java.util.concurrent.Callable<String>() {
            public String call() {
                GLWActivity a = act;
                return a != null && a.tvIsTuned() ? "1" : "0";
            }
        });
        return "1".equals(s) ? 1 : 0;
    }

    // ---- playback controls — forwarded to the TvView in GLWActivity ----

    public static void pause(final boolean paused) {
        GLWActivity a = act;
        if (a == null)
            return;
        a.runOnUiThread(new Runnable() {
            public void run() { a.tvPause(paused); }
        });
    }

    public static void seekTo(final long ms) {
        GLWActivity a = act;
        if (a == null)
            return;
        a.runOnUiThread(new Runnable() {
            public void run() { a.tvSeekTo(ms); }
        });
    }

    // uiCall — run a callable on the UI thread, blocking briefly for the
    // result. JNI callers run on Go threads and can afford a short wait.
    private static String uiCall(final java.util.concurrent.Callable<String> c) {
        GLWActivity a = act;
        if (a == null)
            return "";
        java.util.concurrent.FutureTask<String> t =
            new java.util.concurrent.FutureTask<String>(c);
        a.runOnUiThread(t);
        try {
            return t.get(2, java.util.concurrent.TimeUnit.SECONDS);
        } catch (Exception e) {
            return "";
        }
    }

    // timeshift — "start;cur" wall-clock ms, "" when not playing.
    public static String timeshift() {
        return uiCall(new java.util.concurrent.Callable<String>() {
            public String call() {
                GLWActivity a = act;
                return a == null ? "" : a.tvTimeshift();
            }
        });
    }

    // tracks — JSON array of audio/subtitle tracks, "" when not playing.
    public static String tracks() {
        return uiCall(new java.util.concurrent.Callable<String>() {
            public String call() {
                GLWActivity a = act;
                return a == null ? "[]" : a.tvTracks();
            }
        });
    }

    public static void selectTrack(final int type, final String id) {
        GLWActivity a = act;
        if (a == null)
            return;
        a.runOnUiThread(new Runnable() {
            public void run() { a.tvSelectTrack(type, id); }
        });
    }

    // openSetup — launches the input's own setup activity (declared in
    // its tv-input metadata). Used when the channel DB is empty, e.g.
    // first DVB-T scan. Starts with NEW_TASK — ctx is a service.
    public static boolean openSetup(String inputId) {
        TvInputManager m = tvInputManager();
        if (m == null)
            return false;
        try {
            for (TvInputInfo i : m.getTvInputList()) {
                if (!i.getId().equals(inputId))
                    continue;
                Intent s = i.createSetupIntent();
                if (s == null)
                    return false;
                s.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
                ctx.startActivity(s);
                return true;
            }
        } catch (Exception e) {
            Log.e("Movian", "Tv.openSetup failed", e);
        }
        return false;
    }
}
