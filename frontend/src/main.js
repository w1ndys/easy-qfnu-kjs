import { createApp } from 'vue'
import App from './App.vue'
import router from './router'

// Vant 基础样式
import 'vant/lib/index.css'

// 项目全局样式（覆盖 Vant 主题变量）
import './assets/css/main.css'

const app = createApp(App)
app.use(router)
app.mount('#app')
