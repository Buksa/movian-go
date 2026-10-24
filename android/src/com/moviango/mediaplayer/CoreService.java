package com.moviango.mediaplayer;

import java.io.File;
import android.net.Uri;

import android.app.Service;
import android.content.Intent;
import android.os.Bundle;
import android.os.IBinder;
import android.content.IntentFilter;
import android.content.BroadcastReceiver;
import android.content.Context;
import android.net.ConnectivityManager;
import android.os.Build;
import android.util.Log;

import android.media.AudioManager;

public class CoreService extends Service {

    final IntentFilter networkChecker = new IntentFilter();

    // Registered only once the core is ready, so the callback can
    // never touch class Core early.
    private BroadcastReceiver networkCheckerAction = new BroadcastReceiver() {
            @Override
            public void onReceive(final Context context, final Intent intent) {
                if(CoreBoot.isReady())
                    Core.networkStatusChanged();
            }
        };

    private boolean receiverRegistered;

    private final Runnable registerNetworkReceiver = new Runnable() {
            public void run() {
                if(receiverRegistered)
                    return;
                receiverRegistered = true;
                registerReceiver(networkCheckerAction, networkChecker);
            }
        };

    @Override
    public void onCreate() {
        Log.d("Movian", "Coreservice Init");

        networkChecker.addAction(android.net.ConnectivityManager.CONNECTIVITY_ACTION);
        CoreBoot.start(this);
        CoreBoot.runWhenReady(registerNetworkReceiver);
    }

    @Override
    public void onDestroy() {
        CoreBoot.removeReadyListener(registerNetworkReceiver);
        if(receiverRegistered) {
            receiverRegistered = false;
            unregisterReceiver(networkCheckerAction);
        }
        super.onDestroy();
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        return START_NOT_STICKY;
    }

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }

    public int getSystemAudioSampleRate() {

        if(Build.VERSION.SDK_INT >= 21) {
            AudioManager audioManager = (AudioManager) this.getSystemService(Context.AUDIO_SERVICE);
            String sr = audioManager.getProperty(AudioManager.PROPERTY_OUTPUT_SAMPLE_RATE);
            return Integer.parseInt(sr);
        }
        return 0;
    }

    public int getSystemAudioFramesPerBuffer() {

        if(Build.VERSION.SDK_INT >= 21) {
            AudioManager audioManager = (AudioManager) this.getSystemService(Context.AUDIO_SERVICE);
            String sr = audioManager.getProperty(AudioManager.PROPERTY_OUTPUT_FRAMES_PER_BUFFER);
            return Integer.parseInt(sr);
        }
        return 0;
    }
}

