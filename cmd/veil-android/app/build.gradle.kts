plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

val veilVersion = rootProject.file("../../VERSION").readText().trim()
val veilParts = veilVersion.split(".").map { it.toInt() }

android {
    namespace = "net.pythonscratcher.veil"
    compileSdk = 34

    defaultConfig {
        applicationId = "net.pythonscratcher.veil"
        minSdk = 24
        targetSdk = 34
        versionCode = veilParts[0] * 10000 + veilParts[1] * 100 + veilParts[2]
        versionName = veilVersion
        ndk { abiFilters += listOf("arm64-v8a", "armeabi-v7a") }
    }

    buildFeatures {
        compose = true
        buildConfig = true
    }
    composeOptions {
        kotlinCompilerExtensionVersion = "1.5.14"
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions {
        jvmTarget = "17"
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            signingConfig = signingConfigs.getByName("debug")
        }
    }

    packaging {
        resources {
            excludes += "/META-INF/{AL2.0,LGPL2.1}"
        }
    }
}

dependencies {
    implementation(files("libs/veil.aar"))

    implementation("androidx.core:core-ktx:1.13.1")
    implementation("androidx.activity:activity-compose:1.9.2")
    implementation("androidx.lifecycle:lifecycle-runtime-ktx:2.8.6")
    implementation(platform("androidx.compose:compose-bom:2024.09.03"))
    implementation("androidx.compose.ui:ui")
    implementation("androidx.compose.ui:ui-graphics")
    implementation("androidx.compose.ui:ui-tooling-preview")
    implementation("androidx.compose.material3:material3")
    implementation("androidx.compose.material:material-icons-extended")
    implementation("com.journeyapps:zxing-android-embedded:4.3.0")
}
