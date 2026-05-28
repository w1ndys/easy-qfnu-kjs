import { createApp } from 'vue'
import App from './App.vue'
import router from './router'

// Vant 基础样式（前台用户页面）
import 'vant/lib/index.css'

// Element Plus 命令式组件样式（后台 ElMessage / ElMessageBox 等）
import 'element-plus/es/components/message/style/css'
import 'element-plus/es/components/message-box/style/css'

// 项目全局样式（覆盖 Vant 与 Element Plus 主题变量）
import './assets/css/main.css'

const app = createApp(App)
app.use(router)
app.mount('#app')
