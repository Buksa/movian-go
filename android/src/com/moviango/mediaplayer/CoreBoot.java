package com.moviango.mediaplayer;

import android.os.Handler;
import android.os.Looper;
import android.util.Log;

import java.util.ArrayList;
import java.util.LinkedHashSet;

// CoreBoot — starts the native core on a background Java thread and
// gates everything that needs it until it is ready.
//
// Loading libcore.so (Core's static initializer) runs every Go package
// init() and can take many seconds on slow or translated-ARM devices;
// doing it in CoreService.onCreate on the main thread caused a
// "Timeout executing service" ANR. The thread must be a Java thread:
// coreInit calls FindClass for Core/Apps/Tv, which only resolves app
// classes with the app class loader.
//
// Rule for main-thread code: do not touch class Core (any member, it
// would run its static initializer and block on the class-init lock
// held by the init thread) until isReady() is true. CoreBoot itself
// references Core only from the init thread and after ready. All
// methods except isReady() must be called on the main thread.
final class CoreBoot {

    private static final String TAG = "Movian";

    private static final Handler mainHandler =
        new Handler(Looper.getMainLooper());

    private static boolean started;                    // main thread
    private static volatile boolean ready;
    private static final ArrayList<Runnable> listeners =
        new ArrayList<Runnable>();                     // main thread
    // Deduplicated, ordered: a URI queued by several resumes or
    // lifecycle instances before ready is delivered once.
    private static final LinkedHashSet<String> pendingUris =
        new LinkedHashSet<String>();                   // main thread

    private CoreBoot() {}

    static boolean isReady() {
        return ready;
    }

    // Idempotent: only the first call starts the init thread.
    static void start(final CoreService svc) {
        if (started)
            return;
        started = true;

        Runnable init = new Runnable() {
                public void run() {
                    long t0 = System.currentTimeMillis();
                    try {
                        Core.init(svc);
                    } catch (Throwable t) {
                        Log.e(TAG, "Native core init failed", t);
                        throw new RuntimeException(t);
                    }
                    Log.d(TAG, "Native core ready after " +
                          (System.currentTimeMillis() - t0) + " ms");
                    mainHandler.post(new Runnable() {
                            public void run() { markReady(); }
                        });
                }
            };
        // 8 MB stack like the main thread had: Go runs on this thread's
        // stack inside the JNI call.
        Thread th = new Thread(null, init, "movian-core-init", 8 << 20);
        th.start();
    }

    private static void markReady() {
        ready = true;
        // Listeners first (GLW creation), then queued URIs, matching
        // the order the main looper gave before.
        ArrayList<Runnable> l = new ArrayList<Runnable>(listeners);
        listeners.clear();
        for (Runnable r : l)
            r.run();
        ArrayList<String> uris = new ArrayList<String>(pendingUris);
        pendingUris.clear();
        for (String u : uris)
            Core.openUri(u);
    }

    // Run r on the main thread once the core is ready (immediately if
    // it already is). A given Runnable is queued at most once; remove
    // it with removeReadyListener when its owner goes away.
    static void runWhenReady(Runnable r) {
        if (ready) {
            r.run();
            return;
        }
        if (!listeners.contains(r))
            listeners.add(r);
    }

    static void removeReadyListener(Runnable r) {
        listeners.remove(r);
    }

    static void openUri(String uri) {
        if (ready)
            Core.openUri(uri);
        else
            pendingUris.add(uri);
    }
}
